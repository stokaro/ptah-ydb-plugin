package devdatabase

// White-box testing required: endpointHost decides whether the container
// runtime is on this machine, and the Docker command line cannot be pointed at
// a remote daemon from a unit test.

import (
	"testing"

	qt "github.com/frankban/quicktest"
)

func TestEndpointHost(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, host string
		remote               bool
	}{
		{"unset", "", "", false},
		{"socket", "unix:///var/run/docker.sock", "", false},
		{"named pipe", "npipe:////./pipe/docker_engine", "", false},
		{"loopback address", "tcp://127.0.0.1:2375", "127.0.0.1", false},
		{"loopback name", "tcp://localhost:2375", "localhost", false},
		{"IPv6 loopback", "tcp://[::1]:2375", "::1", false},
		{"remote over ssh", "ssh://buster@remote-dev", "remote-dev", true},
		{"remote over tcp", "tcp://192.0.2.10:2375", "192.0.2.10", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			host, remote, err := endpointHost(test.endpoint)

			c.Assert(err, qt.IsNil)
			c.Assert(host, qt.Equals, test.host)
			c.Assert(remote, qt.Equals, test.remote)
		})
	}
}

func TestEndpointHost_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, endpoint string }{
		{"no host", "tcp://"},
		{"unreadable", "tcp://host:%zz"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			_, _, err := endpointHost(test.endpoint)

			c.Assert(err, qt.IsNotNil)
		})
	}
}
