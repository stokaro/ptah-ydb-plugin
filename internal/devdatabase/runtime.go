package devdatabase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// Runtime starts and removes containers on the machine the provider runs on.
type Runtime interface {
	// Available returns nil when containers can be started here, a Refusal
	// otherwise. The image tag appears in a refusal that names the image.
	Available(ctx context.Context, image string) error
	// Start runs the image detached with env set and its port published on
	// loopback, and returns the host:port the port is published on.
	Start(ctx context.Context, name, image string, env []string, port string) (hostPort string, err error)
	// Running reports whether the container is still running.
	Running(ctx context.Context, name string) (bool, error)
	// Remove force-removes the container. A container that does not exist is
	// removed already.
	Remove(ctx context.Context, name string) error
}

// ContainerLabel marks every container the provider starts, so an operator can
// find one a killed process left behind:
// docker ps -aq --filter label=ptah-dev
const ContainerLabel = "ptah-dev"

// DockerCLI is the Runtime that drives the docker command line. It honors
// DOCKER_HOST and the active Docker context, as the command line does.
type DockerCLI struct {
	// RunLabel is the value of the container label, unique to one provider
	// process.
	RunLabel string
}

func (DockerCLI) docker(ctx context.Context, args ...string) (string, string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

// Available checks that the daemon answers and runs on this machine. local-ydb
// takes every connection without a credential, so a daemon on another machine
// would publish it on an interface other peers reach.
func (d DockerCLI) Available(ctx context.Context, image string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return &Refusal{Unavailable: true, Reason: "the docker command is not on PATH; the provider runs with an explicit environment, so pass PATH, and DOCKER_HOST or DOCKER_CONTEXT when the daemon is not the default, to it"}
	}
	if _, _, err := d.docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return &Refusal{Unavailable: true, Reason: "the container runtime does not answer; start the Docker daemon"}
	}
	raw := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if raw == "" {
		out, _, err := d.docker(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return &Refusal{Reason: "docker://ydb starts a server that takes connections without a credential, and the container runtime cannot say whether it runs on this machine"}
		}
		raw = strings.TrimSpace(out)
	}
	host, remote, err := endpointHost(raw)
	if err != nil {
		return &Refusal{Reason: "docker://ydb starts a server that takes connections without a credential, and the container runtime endpoint cannot be read"}
	}
	if remote {
		return &Refusal{Reason: fmt.Sprintf("docker://ydb starts %s, which takes every connection without a credential, and the container runtime runs on %s, "+
			"where the database would be published on every interface for the life of the command; point DOCKER_HOST at a container runtime on this machine, "+
			"or pass a directly connectable dev database URL", image, host)}
	}
	return nil
}

// endpointHost classifies a docker endpoint. An empty value, a socket and a
// pipe are this machine, and so is a loopback address.
func endpointHost(raw string) (host string, remote bool, err error) {
	if raw == "" {
		return "", false, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, errors.New("unreadable docker endpoint")
	}
	switch parsed.Scheme {
	case "unix", "npipe", "fd", "":
		return "", false, nil
	}
	host = parsed.Hostname()
	if host == "" {
		return "", false, errors.New("docker endpoint names no host")
	}
	if strings.EqualFold(host, "localhost") {
		return host, false, nil
	}
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil && addr.IsLoopback() {
		return host, false, nil
	}
	return host, true, nil
}

// Start runs the container and reads the port the daemon chose.
func (d DockerCLI) Start(ctx context.Context, name, image string, env []string, port string) (string, error) {
	args := []string{"run", "--detach", "--name", name, "--label", ContainerLabel + "=" + d.RunLabel, "--publish", "127.0.0.1::" + port}
	for _, entry := range env {
		args = append(args, "--env", entry)
	}
	args = append(args, image)
	if _, stderr, err := d.docker(ctx, args...); err != nil {
		return "", fmt.Errorf("docker run failed: %s", firstLine(stderr, err))
	}
	out, stderr, err := d.docker(ctx, "port", name, port+"/tcp")
	if err != nil {
		return "", fmt.Errorf("docker port failed: %s", firstLine(stderr, err))
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	host, publishedPort, err := net.SplitHostPort(strings.TrimSpace(line))
	if err != nil || host != "127.0.0.1" {
		return "", errors.New("the container runtime did not publish the port on loopback")
	}
	return net.JoinHostPort(host, publishedPort), nil
}

// Running asks the daemon for the container's state.
func (d DockerCLI) Running(ctx context.Context, name string) (bool, error) {
	out, stderr, err := d.docker(ctx, "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, fmt.Errorf("docker inspect failed: %s", firstLine(stderr, err))
	}
	return strings.TrimSpace(out) == "true", nil
}

// Remove force-removes the container.
func (d DockerCLI) Remove(ctx context.Context, name string) error {
	_, stderr, err := d.docker(ctx, "rm", "--force", "--volumes", name)
	if err != nil && !strings.Contains(stderr, "No such container") {
		return fmt.Errorf("docker rm failed: %s", firstLine(stderr, err))
	}
	return nil
}

// firstLine is the first line of the daemon's answer, or the error text.
func firstLine(stderr string, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	if line == "" {
		return err.Error()
	}
	return line
}
