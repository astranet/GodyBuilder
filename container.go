package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type Container interface {
	ID() string
	Name() string

	Start() error
	Stop(dur time.Duration) error
	Remove() error

	Exec(ctx context.Context, cmd string, args ...string) (string, error)
	State() (ContainerState, error)
}

type ContainerState int

const (
	StateNone    ContainerState = 0
	StateRunning ContainerState = 1
	StatePending ContainerState = 2
	StateStopped ContainerState = 3
)

const (
	defaultRPCTimeout     = 10 * time.Second
	defaultPendingTimeout = time.Minute
	defaultExecPolling    = 500 * time.Millisecond
	defaultOutputPolling  = 500 * time.Millisecond
)

func NewContainer(name, image string,
	hostMounts, env map[string]string) (Container, error) {

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.39"))
	if err != nil {
		return nil, err
	}
	if len(image) > 0 {
		if !checkImage(cli, image) {
			log.Println("gody: trying to pull", image)
			if err := pullImage(cli, image); err != nil {
				return nil, err
			}
		}
	}
	return &dockerContainer{
		cli:        cli,
		id:         name,
		name:       name,
		image:      image,
		rpcTimeout: defaultRPCTimeout,
		hostMounts: hostMounts,
		env:        env,
	}, nil
}

type dockerContainer struct {
	id         string
	cli        *client.Client
	name       string
	image      string
	rpcTimeout time.Duration
	offline    bool
	hostMounts map[string]string
	env        map[string]string
}

func (d *dockerContainer) Start() error {
	state, err := d.State()
	if err != nil {
		return err
	} else if state == StateRunning {
		return nil
	}
	switch state {
	case StateNone:
		if err := d.createContainer(d.hostMounts, d.env); err != nil {
			return err
		}
		state, err := d.State()
		if err != nil {
			return fmt.Errorf("failed to read state after container creation: %v", err)
		}
		if state == StatePending {
			if err := d.waitPendingFor(defaultPendingTimeout); err != nil {
				return fmt.Errorf("container is not being created for too long: %v", err)
			}
		}
		return d.Start()
	case StateStopped:
		ctx, cancelFn := context.WithTimeout(context.Background(), d.rpcTimeout)
		defer cancelFn()
		err := d.cli.ContainerStart(ctx, d.name, types.ContainerStartOptions{})
		if err != nil {
			return err
		}
		state, err := d.State()
		if err != nil {
			return fmt.Errorf("failed to read state after container start: %v", err)
		}
		if state == StatePending {
			if err := d.waitPendingFor(defaultPendingTimeout); err != nil {
				return fmt.Errorf("container starts too long: %v", err)
			}
		}
		return nil
	case StatePending:
		if err := d.waitPendingFor(defaultPendingTimeout); err != nil {
			return err
		}
		return d.Start()
	default:
		return fmt.Errorf("unexpected container state: %v", state)
	}

	return nil
}

func (d *dockerContainer) createContainer(hostMounts, env map[string]string) error {
	ctx, cancelFn := context.WithTimeout(context.Background(), d.rpcTimeout)
	defer cancelFn()

	envSpecs := make([]string, 0, len(env))
	for k, v := range env {
		envSpecs = append(envSpecs, fmt.Sprintf("%s=%s", k, v))
	}
	config := &container.Config{
		Image:        d.image,
		AttachStdout: true,
		AttachStderr: true,
		Env:          envSpecs,
	}
	mountSpecs := make([]mount.Mount, 0, len(hostMounts))
	for src, dst := range hostMounts {
		mountSpecs = append(mountSpecs, mount.Mount{
			Type:   mount.TypeBind,
			Source: src,
			Target: dst,
		})
	}
	hostConfig := &container.HostConfig{
		Mounts: mountSpecs,
	}
	networkingConfig := &network.NetworkingConfig{}
	resp, err := d.cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, d.name)
	if err != nil {
		return err
	}
	for _, w := range resp.Warnings {
		log.Println("docker warn:", w)
	}
	d.id = resp.ID
	if len(d.name) == 0 {
		d.name = resp.ID
	}
	return nil
}

func (d *dockerContainer) Stop(dur time.Duration) error {
	ctx, cancelFn := context.WithTimeout(context.Background(), d.rpcTimeout)
	defer cancelFn()

	if err := d.cli.ContainerStop(ctx, d.name, &dur); err != nil {
		return err
	}
	state, err := d.State()
	if err != nil {
		return fmt.Errorf("failed to read state after container stop: %v", err)
	}
	if state == StatePending {
		if err = d.waitPendingFor(defaultPendingTimeout); err != nil {
			return fmt.Errorf("container is pendig after stop signal for too long: %v", err)
		}
	}
	return nil
}

func (d *dockerContainer) Exec(ctx context.Context, cmd string, args ...string) (string, error) {
	exe, err := d.cli.ContainerExecCreate(ctx, d.name, types.ExecConfig{
		AttachStdout: true,
		AttachStderr: true,

		Cmd: append([]string{cmd}, args...),
	})
	if err != nil {
		return "", fmt.Errorf("docker failed to init command: %v", err)
	}
	resp, err := d.cli.ContainerExecAttach(ctx, exe.ID, types.ExecStartCheck{})
	if err != nil {
		return "", fmt.Errorf("docker failed to attach to command: %v", err)
	}

	output := newSafeBuffer()
	outWait := new(sync.WaitGroup)
	outWait.Add(1)
	go func() {
		defer outWait.Done()
		defer resp.Close()
		stdOut, stdErr := outputMux(ctx, output)
		stdcopy.StdCopy(
			io.MultiWriter(os.Stdout, stdOut),
			io.MultiWriter(os.Stderr, stdErr),
			resp.Reader,
		)
	}()

	doneC := make(chan int, 1)
	go func() {
		t := time.NewTimer(defaultExecPolling)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				status, err := d.cli.ContainerExecInspect(ctx, exe.ID)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Println("docker failed to inspect exec:", err)
					t.Reset(time.Second)
					continue
				}
				if status.Running {
					t.Reset(defaultExecPolling)
					continue
				}
				doneC <- status.ExitCode
			}
		}
	}()

	// P.S. there is no way to kill Exec job once started, see:
	// https://github.com/moby/moby/issues/9098
	if err := d.cli.ContainerExecStart(ctx, exe.ID, types.ExecStartCheck{}); err != nil {
		return "", fmt.Errorf("docker failed to run command: %v", err)
	}
	select {
	case status := <-doneC:
		outWait.Wait()
		if status != 0 {
			return output.String(), fmt.Errorf("exit status %d", status)
		}
	case <-ctx.Done():
		return output.String(), ctx.Err()
	}
	return output.String(), nil
}

func (d *dockerContainer) Remove() error {
	ctx, cancelFn := context.WithTimeout(context.Background(), d.rpcTimeout)
	defer cancelFn()

	return d.cli.ContainerRemove(ctx, d.name, types.ContainerRemoveOptions{
		Force: true,
	})
}

func (d *dockerContainer) State() (ContainerState, error) {
	ctx, cancelFn := context.WithTimeout(context.Background(), d.rpcTimeout)
	defer cancelFn()
	info, err := d.cli.ContainerInspect(ctx, d.name)
	if err != nil {
		if client.IsErrNotFound(err) {
			return StateNone, nil
		}
		return StateNone, err
	}
	switch info.State.Status {
	case "running":
		return StateRunning, nil
	case "created", "exited", "dead", "paused":
		return StateStopped, nil
	default: //  "restarting", "removing", i.e. non-term
		return StatePending, nil
	}
}

func (d *dockerContainer) wait(state ContainerState, dur time.Duration) error {
	var stateCond container.WaitCondition
	switch state {
	case StateNone:
		stateCond = container.WaitConditionRemoved
	case StateStopped:
		stateCond = container.WaitConditionNotRunning
	default:
		return nil
	}
	if d.rpcTimeout > dur {
		dur = d.rpcTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	_, errC := d.cli.ContainerWait(ctx, d.name, stateCond)
	if err := <-errC; err != nil {
		return err
	}
	return nil
}

func (d *dockerContainer) waitPendingFor(dur time.Duration) error {
	ctx, cancelFn := context.WithTimeout(context.Background(), dur)
	defer cancelFn()
	state, err := StatePending, error(nil)
	for state == StatePending {
		time.Sleep(time.Second)
		if state, err = d.State(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.New("container state is pending too long")
		default:
		}
	}
	return nil
}

func (d *dockerContainer) ID() string {
	return d.id
}

func (d *dockerContainer) Name() string {
	return d.name
}

func checkImage(cli *client.Client, refStr string) bool {
	ctx, cancelFn := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancelFn()

	list, err := cli.ImageList(ctx, types.ImageListOptions{
		All: true,
	})
	if err != nil {
		return false
	}
	var imageTag string
	{
		parts := strings.Split(refStr, ":")
		imageTag = parts[0]
	}
	for _, img := range list {
		for _, ref := range img.RepoTags {
			parts := strings.Split(ref, ":")
			if parts[0] == imageTag {
				return true
			}
		}
	}
	return false
}

func pullImage(cli *client.Client, refStr string) error {
	ctx, cancelFn := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancelFn()

	resp, err := cli.ImagePull(ctx, refStr, types.ImagePullOptions{})
	if err != nil {
		return err
	}
	io.Copy(ioutil.Discard, resp)
	resp.Close()
	return nil
}

type Restarter interface {
	RestartRx(pattern *regexp.Regexp, dur time.Duration) (int, error)
}

func NewRestarter() (Restarter, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.39"))
	if err != nil {
		return nil, err
	}
	return &restarter{
		cli:        cli,
		rpcTimeout: defaultRPCTimeout,
	}, nil
}

func (r *restarter) RestartRx(pattern *regexp.Regexp, dur time.Duration) (int, error) {
	listCtx, listCancel := context.WithTimeout(context.Background(), r.rpcTimeout)
	defer listCancel()
	list, err := r.cli.ContainerList(listCtx, types.ContainerListOptions{})
	if err != nil {
		return 0, err
	}
	toRestart := make([]string, 0, len(list))
	for _, c := range list {
		for _, name := range c.Names {
			if pattern.MatchString(name) {
				toRestart = append(toRestart, name)
			}
		}
	}
	sort.Strings(toRestart)
	var restarted int
	for _, name := range toRestart {
		restartCtx, restartCancel := context.WithTimeout(context.Background(), r.rpcTimeout)
		defer restartCancel()
		if err := r.cli.ContainerRestart(restartCtx, name, &dur); err != nil {
			log.Printf("failed to restart %s, error: %v", name, err)
			continue
		}
		restarted++
	}
	return restarted, nil
}

type restarter struct {
	cli        *client.Client
	rpcTimeout time.Duration
}
