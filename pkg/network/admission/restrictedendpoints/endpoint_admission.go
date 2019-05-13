package restrictedendpoints

import (
	"fmt"
	"io"
	"net"
	"reflect"

	"k8s.io/klog"

	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	kapi "k8s.io/kubernetes/pkg/apis/core"

	configlatest "github.com/openshift/origin/pkg/cmd/server/apis/config/latest"
	"github.com/openshift/origin/pkg/network/admission/apis/restrictedendpoints"
)

const RestrictedEndpointsPluginName = "network.openshift.io/RestrictedEndpointsAdmission"

func RegisterRestrictedEndpoints(plugins *admission.Plugins) {
	plugins.Register(RestrictedEndpointsPluginName,
		func(config io.Reader) (admission.Interface, error) {
			pluginConfig, err := readConfig(config)
			if err != nil {
				return nil, err
			}
			if pluginConfig == nil {
				klog.Infof("Admission plugin %q is not configured so it will be disabled.", RestrictedEndpointsPluginName)
				return nil, nil
			}
			restrictedNetworks, err := ParseSimpleCIDRRules(pluginConfig.RestrictedCIDRs)
			if err != nil {
				// should have been caught with validation
				return nil, err
			}

			return NewRestrictedEndpointsAdmission(restrictedNetworks), nil
		})
}

func readConfig(reader io.Reader) (*restrictedendpoints.RestrictedEndpointsAdmissionConfig, error) {
	if reader == nil || reflect.ValueOf(reader).IsNil() {
		return nil, nil
	}
	obj, err := configlatest.ReadYAML(reader)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}
	config, ok := obj.(*restrictedendpoints.RestrictedEndpointsAdmissionConfig)
	if !ok {
		return nil, fmt.Errorf("unexpected config object: %#v", obj)
	}
	// No validation needed since config is just list of strings
	return config, nil
}

type restrictedEndpointsAdmission struct {
	*admission.Handler

	authorizer         authorizer.Authorizer
	restrictedNetworks []*net.IPNet
	restrictedPorts    []kapi.EndpointPort
}

var _ = initializer.WantsAuthorizer(&restrictedEndpointsAdmission{})
var _ = admission.ValidationInterface(&restrictedEndpointsAdmission{})

// ParseSimpleCIDRRules parses a list of CIDR strings
func ParseSimpleCIDRRules(rules []string) (networks []*net.IPNet, err error) {
	for _, s := range rules {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		networks = append(networks, cidr)
	}
	return networks, nil
}

// NewRestrictedEndpointsAdmission creates a new endpoints admission plugin.
func NewRestrictedEndpointsAdmission(restrictedNetworks []*net.IPNet) *restrictedEndpointsAdmission {
	return &restrictedEndpointsAdmission{
		Handler:            admission.NewHandler(admission.Create, admission.Update),
		restrictedNetworks: restrictedNetworks,
		restrictedPorts: []kapi.EndpointPort{
			{Protocol: kapi.ProtocolTCP, Port: 22623},
			{Protocol: kapi.ProtocolTCP, Port: 22624},
		},
	}
}

func (r *restrictedEndpointsAdmission) SetAuthorizer(a authorizer.Authorizer) {
	r.authorizer = a
}

func (r *restrictedEndpointsAdmission) ValidateInitialization() error {
	if r.authorizer == nil {
		return fmt.Errorf("missing authorizer")
	}
	return nil
}

func (r *restrictedEndpointsAdmission) findRestrictedIP(ep *kapi.Endpoints) error {
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			ip := net.ParseIP(addr.IP)
			if ip == nil {
				continue
			}
			for _, net := range r.restrictedNetworks {
				if net.Contains(ip) {
					return fmt.Errorf("endpoint address %s is not allowed", addr.IP)
				}
			}
		}
	}
	return nil
}

func (r *restrictedEndpointsAdmission) findRestrictedPort(ep *kapi.Endpoints) error {
	for _, subset := range ep.Subsets {
		for _, port := range subset.Ports {
			for _, restricted := range r.restrictedPorts {
				if port.Protocol == restricted.Protocol && port.Port == restricted.Port {
					return fmt.Errorf("endpoint port %s:%d is not allowed", string(port.Protocol), port.Port)
				}
			}
		}
	}
	return nil
}

func (r *restrictedEndpointsAdmission) checkAccess(attr admission.Attributes) (bool, error) {
	authzAttr := authorizer.AttributesRecord{
		User:            attr.GetUserInfo(),
		Verb:            "create",
		Namespace:       attr.GetNamespace(),
		Resource:        "endpoints",
		Subresource:     "restricted",
		APIGroup:        kapi.GroupName,
		Name:            attr.GetName(),
		ResourceRequest: true,
	}
	authorized, _, err := r.authorizer.Authorize(authzAttr)
	return authorized == authorizer.DecisionAllow, err
}

// Admit determines if the endpoints object should be admitted
func (r *restrictedEndpointsAdmission) Validate(a admission.Attributes) error {
	if a.GetResource().GroupResource() != kapi.Resource("endpoints") {
		return nil
	}
	ep, ok := a.GetObject().(*kapi.Endpoints)
	if !ok {
		return nil
	}
	old, ok := a.GetOldObject().(*kapi.Endpoints)
	if ok && reflect.DeepEqual(ep.Subsets, old.Subsets) {
		return nil
	}

	restrictedErr := r.findRestrictedIP(ep)
	if restrictedErr == nil {
		restrictedErr = r.findRestrictedPort(ep)
	}
	if restrictedErr == nil {
		return nil
	}

	allow, err := r.checkAccess(a)
	if err != nil {
		return err
	}
	if !allow {
		return admission.NewForbidden(a, restrictedErr)
	}
	return nil
}
