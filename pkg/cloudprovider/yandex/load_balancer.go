package yandex

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/loadbalancer/v1"
	v1 "k8s.io/api/core/v1"
	svchelpers "k8s.io/cloud-provider/service/helpers"
	"k8s.io/kubernetes/pkg/master/ports"
)

const (
	targetGroupNetworkIdAnnotation = "yandex.cpi.flant.com/target-group-network-id"
	externalLoadBalancerAnnotation = "yandex.cpi.flant.com/loadbalancer-external"
	listenerSubnetIdAnnotation     = "yandex.cpi.flant.com/listener-subnet-id"
	listenerAddressIPv4            = "yandex.cpi.flant.com/listener-address-ipv4"
)

type LoadBalancerService interface {
	LoadBalancerManager
	TargetGroupManager
}

type LoadBalancerManager interface {
	CreateOrUpdateLB(ctx context.Context, id string, listenerSpec []*loadbalancer.ListenerSpec, attachedTGs []*loadbalancer.AttachedTargetGroup) (*v1.LoadBalancerStatus, error)
	GetLbByName(ctx context.Context, name string) (*v1.LoadBalancerStatus, bool, error)
	RemoveLB(ctx context.Context, id string) error
}

type TargetGroupManager interface {
	CreateOrUpdateTG(ctx context.Context, LbName string, targets []*loadbalancer.Target) (string, error)
	GetTgByName(ctx context.Context, name string) (*loadbalancer.TargetGroup, error)
	RemoveTG(ctx context.Context, name string) error
}

var kubeToYandexServiceProtoMapping = map[v1.Protocol]loadbalancer.Listener_Protocol{
	v1.ProtocolTCP: loadbalancer.Listener_TCP,
	v1.ProtocolUDP: loadbalancer.Listener_UDP,
}

func (yc *Cloud) GetLoadBalancer(ctx context.Context, _ string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	lbName := defaultLoadBalancerName(service)

	return yc.api.LbSvc.GetLbByName(ctx, lbName)
}

func (yc *Cloud) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	return defaultLoadBalancerName(service)
}

func (yc *Cloud) EnsureLoadBalancer(ctx context.Context, _ string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	return yc.ensureLB(ctx, service, nodes)
}

func (yc *Cloud) UpdateLoadBalancer(ctx context.Context, _ string, service *v1.Service, nodes []*v1.Node) error {
	_, err := yc.ensureLB(ctx, service, nodes)
	return err
}

func (yc *Cloud) EnsureLoadBalancerDeleted(ctx context.Context, _ string, service *v1.Service) error {
	lbName := defaultLoadBalancerName(service)

	err := yc.api.LbSvc.RemoveLB(ctx, lbName)
	if err != nil {
		return err
	}

	return err
}

func defaultLoadBalancerName(service *v1.Service) string {
	ret := "a" + string(service.UID)
	ret = strings.Replace(ret, "-", "", -1)
	if len(ret) > 32 {
		ret = ret[:32]
	}
	return ret
}

func (yc *Cloud) ensureLB(ctx context.Context, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	// sanity checks
	// current API restrictions
	if len(service.Spec.Ports) > 10 {
		return nil, fmt.Errorf("Yandex.Cloud API does not support more than 10 listener port specifications")
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no Nodes provided")
	}

	lbName := defaultLoadBalancerName(service)
	lbParams := yc.getLoadBalancerParameters(service)

	var listenerSpecs []*loadbalancer.ListenerSpec
	for index, svcPort := range service.Spec.Ports {
		listenerName := svcPort.Name
		if len(listenerName) == 0 {
			listenerName = "listener-" + strconv.Itoa(index)
		}

		listenerSpec := &loadbalancer.ListenerSpec{
			Name:       listenerName,
			Port:       int64(svcPort.Port),
			Protocol:   kubeToYandexServiceProtoMapping[svcPort.Protocol],
			TargetPort: int64(svcPort.NodePort),
		}

		if lbParams.internal {
			internalAddressSpec := &loadbalancer.InternalAddressSpec{
				SubnetId: lbParams.listenerSubnetID,
			}

			if len(lbParams.listenerAddressIPv4) > 0 {
				internalAddressSpec.Address = lbParams.listenerAddressIPv4
				internalAddressSpec.IpVersion = loadbalancer.IpVersion_IPV4
			}

			listenerSpec.Address = &loadbalancer.ListenerSpec_InternalAddressSpec{
				InternalAddressSpec: internalAddressSpec,
			}
		} else {
			externalAddressSpec := &loadbalancer.ExternalAddressSpec{}

			if len(lbParams.listenerAddressIPv4) > 0 {
				externalAddressSpec.Address = lbParams.listenerAddressIPv4
				externalAddressSpec.IpVersion = loadbalancer.IpVersion_IPV4
			}

			listenerSpec.Address = &loadbalancer.ListenerSpec_ExternalAddressSpec{
				ExternalAddressSpec: externalAddressSpec,
			}
		}

		listenerSpecs = append(listenerSpecs, listenerSpec)
	}

	hcPath, hcPort := "/healthz", int32(ports.ProxyHealthzPort)
	if svchelpers.RequestsOnlyLocalTraffic(service) {
		// Service requires a special health check, retrieve the OnlyLocal port & path
		hcPath, hcPort = svchelpers.GetServiceHealthCheckPathPort(service)
	}

	log.Printf("Health checking on path %q and port %v", hcPath, hcPort)
	healthChecks := []*loadbalancer.HealthCheck{
		{
			Name:               "kube-health-check",
			UnhealthyThreshold: 2,
			HealthyThreshold:   2,
			Options: &loadbalancer.HealthCheck_HttpOptions_{
				HttpOptions: &loadbalancer.HealthCheck_HttpOptions{
					Port: int64(hcPort),
					Path: hcPath,
				},
			},
		},
	}

	tgName := yc.config.ClusterName + lbParams.targetGroupNetworkID
	tg, err := yc.api.LbSvc.GetTgByName(ctx, tgName)
	if err != nil {
		return nil, err
	}
	if tg == nil {
		return nil, fmt.Errorf("TG %q does not exist yet", tgName)
	}

	lbStatus, err := yc.api.LbSvc.CreateOrUpdateLB(ctx, lbName, listenerSpecs, []*loadbalancer.AttachedTargetGroup{
		{
			TargetGroupId: tg.Id,
			HealthChecks:  healthChecks,
		},
	})
	if err != nil {
		return nil, err
	}

	return lbStatus, nil
}

type loadBalancerParameters struct {
	targetGroupNetworkID string
	listenerSubnetID     string
	listenerAddressIPv4  string
	internal             bool
}

func (yc *Cloud) getLoadBalancerParameters(svc *v1.Service) (lbParams loadBalancerParameters) {
	if value, ok := svc.ObjectMeta.Annotations[listenerSubnetIdAnnotation]; ok {
		lbParams.internal = true
		lbParams.listenerSubnetID = value
	} else if len(yc.config.lbListenerSubnetID) != 0 {
		lbParams.listenerSubnetID = yc.config.lbListenerSubnetID
		_, isExternal := svc.ObjectMeta.Annotations[externalLoadBalancerAnnotation]
		lbParams.internal = !isExternal
	}

	if value, ok := svc.ObjectMeta.Annotations[targetGroupNetworkIdAnnotation]; ok {
		lbParams.targetGroupNetworkID = value
	} else if len(yc.config.lbTgNetworkID) != 0 {
		lbParams.targetGroupNetworkID = yc.config.lbTgNetworkID
	}

	if value, ok := svc.ObjectMeta.Annotations[listenerAddressIPv4]; ok {
		lbParams.listenerAddressIPv4 = value
	}

	return
}
