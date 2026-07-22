// Package resolver defines the trusted seam between opaque exposure IDs and
// platform-managed Kubernetes Services.
package resolver

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Resolver returns a constrained HTTP destination for an exposure. Resolver
// implementations must derive this value from trusted platform state, never
// from request data.
type Resolver interface {
	Resolve(context.Context, string) (HTTPServiceTarget, error)
}

// HTTPServiceTarget identifies a Kubernetes Service and port. Its fields are
// deliberately private so a Resolver cannot smuggle an arbitrary URL, path,
// userinfo, query, or protocol through the routing seam.
type HTTPServiceTarget struct {
	service   string
	namespace string
	port      uint16
}

// NewHTTPServiceTarget constructs a cleartext HTTP target for a Kubernetes
// Service. Kubernetes DNS labels and a non-zero port are the entire accepted
// destination vocabulary.
func NewHTTPServiceTarget(service, namespace string, port uint16) (HTTPServiceTarget, error) {
	if errs := validation.IsDNS1123Label(service); len(errs) != 0 {
		return HTTPServiceTarget{}, fmt.Errorf("invalid service name")
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) != 0 {
		return HTTPServiceTarget{}, fmt.Errorf("invalid service namespace")
	}
	if port == 0 {
		return HTTPServiceTarget{}, fmt.Errorf("invalid service port")
	}
	return HTTPServiceTarget{service: service, namespace: namespace, port: port}, nil
}

// Validate rejects the zero value as well as malformed values.
func (t HTTPServiceTarget) Validate() error {
	_, err := NewHTTPServiceTarget(t.service, t.namespace, t.port)
	return err
}

// Hostname is the Kubernetes Service DNS name used by the router.
func (t HTTPServiceTarget) Hostname() string {
	return t.service + "." + t.namespace + ".svc"
}

// Authority is the Service DNS name and port.
func (t HTTPServiceTarget) Authority() string {
	return net.JoinHostPort(t.Hostname(), strconv.Itoa(int(t.port)))
}

// MatchesAuthority reports whether an absolute redirect points back to this
// exact upstream. The default HTTP port may be omitted.
func (t HTTPServiceTarget) MatchesAuthority(authority string) bool {
	return strings.EqualFold(authority, t.Authority()) ||
		(t.port == 80 && strings.EqualFold(authority, t.Hostname()))
}
