package ovn

import (
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

type lbEndpoints struct {
	IPs  []string
	Port int32
}

func (ovn *Controller) getLbEndpoints(ep *kapi.Endpoints, tcpPortMap, udpPortMap map[string]lbEndpoints) {
	for _, s := range ep.Subsets {
		for _, ip := range s.Addresses {
			for _, port := range s.Ports {
				var ips []string
				var portMap map[string]lbEndpoints
				if port.Protocol == kapi.ProtocolUDP {
					portMap = udpPortMap
				} else if port.Protocol == kapi.ProtocolTCP {
					portMap = tcpPortMap
				}
				if lbEps, ok := portMap[port.Name]; ok {
					ips = lbEps.IPs
				} else {
					ips = make([]string, 0)
				}
				ips = append(ips, ip.IP)
				portMap[port.Name] = lbEndpoints{IPs: ips, Port: port.Port}
			}
		}
	}
	logrus.Debugf("Tcp table: %v\nUdp table: %v", tcpPortMap, udpPortMap)
}

// AddEndpoints adds endpoints and creates corresponding resources in OVN
func (ovn *Controller) AddEndpoints(ep *kapi.Endpoints) error {
	// get service
	// TODO: cache the service
	svc, err := ovn.kube.GetService(ep.Namespace, ep.Name)
	if err != nil {
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		logrus.Debugf("no service found for endpoint %s in namespace %s",
			ep.Name, ep.Namespace)
		return nil
	}
	if !util.IsClusterIPSet(svc) {
		logrus.Debugf("Skipping service %s due to clusterIP = %q",
			svc.Name, svc.Spec.ClusterIP)
		return nil
	}
	tcpPortMap := make(map[string]lbEndpoints)
	udpPortMap := make(map[string]lbEndpoints)
	ovn.getLbEndpoints(ep, tcpPortMap, udpPortMap)
	for svcPortName, lbEps := range tcpPortMap {
		ips := lbEps.IPs
		targetPort := lbEps.Port
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolTCP && svcPort.Name == svcPortName {
				if util.ServiceTypeHasNodePort(svc) && config.Gateway.NodeportEnable {
					logrus.Debugf("Creating Gateways IP for NodePort: %d, %v", svcPort.NodePort, ips)
					err = ovn.createGatewaysVIP(string(svcPort.Protocol), svcPort.NodePort, targetPort, ips)
					if err != nil {
						logrus.Errorf("Error in creating Node Port for svc %s, node port: %d - %v\n", svc.Name, svcPort.NodePort, err)
						continue
					}
				}
				if util.ServiceTypeHasClusterIP(svc) {
					var loadBalancer string
					loadBalancer, err = ovn.getLoadBalancer(svcPort.Protocol)
					if err != nil {
						logrus.Errorf("Failed to get loadbalancer for %s (%v)",
							svcPort.Protocol, err)
						continue
					}
					err = ovn.createLoadBalancerVIP(loadBalancer,
						svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
					if err != nil {
						logrus.Errorf("Error in creating Cluster IP for svc %s, target port: %d - %v\n", svc.Name, targetPort, err)
						continue
					}
					ovn.handleExternalIPs(svc, svcPort, ips, targetPort)
				}
			}
		}
	}
	for svcPortName, lbEps := range udpPortMap {
		ips := lbEps.IPs
		targetPort := lbEps.Port
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Protocol == kapi.ProtocolUDP && svcPort.Name == svcPortName {
				if util.ServiceTypeHasNodePort(svc) && config.Gateway.NodeportEnable {
					err = ovn.createGatewaysVIP(string(svcPort.Protocol), svcPort.NodePort, targetPort, ips)
					if err != nil {
						logrus.Errorf("Error in creating Node Port for svc %s, node port: %d - %v\n", svc.Name, svcPort.NodePort, err)
						continue
					}
				}
				if util.ServiceTypeHasClusterIP(svc) {
					var loadBalancer string
					loadBalancer, err = ovn.getLoadBalancer(svcPort.Protocol)
					if err != nil {
						logrus.Errorf("Failed to get loadbalancer for %s (%v)",
							svcPort.Protocol, err)
						continue
					}
					err = ovn.createLoadBalancerVIP(loadBalancer,
						svc.Spec.ClusterIP, svcPort.Port, ips, targetPort)
					if err != nil {
						logrus.Errorf("Error in creating Cluster IP for svc %s, target port: %d - %v\n", svc.Name, targetPort, err)
						continue
					}
					ovn.handleExternalIPs(svc, svcPort, ips, targetPort)
				}
			}
		}
	}
	return nil
}

func (ovn *Controller) handleNodePortLB(node *kapi.Node) {
	physicalGateway := "GR_" + node.Name
	var k8sNSLbTCP, k8sNSLbUDP, physicalIP string
	// Wait for the TCP, UDP north-south load balancers created by the new node.
	if err := wait.Poll(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		if k8sNSLbTCP, _ = ovn.getGatewayLoadBalancer(physicalGateway, TCP); k8sNSLbTCP == "" {
			return false, nil
		}
		if k8sNSLbUDP, _ = ovn.getGatewayLoadBalancer(physicalGateway, UDP); k8sNSLbUDP == "" {
			return false, nil
		}
		if physicalIP, _ = ovn.getGatewayPhysicalIP(physicalGateway); physicalIP == "" {
			return false, nil
		}
		return true, nil
	}); err != nil {
		logrus.Errorf("timed out waiting for load balancer to be ready on node %q", node.Name)
		return
	}
	namespaces, err := ovn.kube.GetNamespaces()
	if err != nil {
		logrus.Errorf("failed to get k8s namespaces: %v", err)
		return
	}
	for _, ns := range namespaces.Items {
		endpoints, err := ovn.kube.GetEndpoints(ns.Name)
		if err != nil {
			logrus.Errorf("failed to get k8s endpoints: %v", err)
			continue
		}
		for _, ep := range endpoints.Items {
			svc, err := ovn.kube.GetService(ep.Namespace, ep.Name)
			if err != nil {
				continue
			}
			if !util.ServiceTypeHasNodePort(svc) {
				continue
			}
			tcpPortMap := make(map[string]lbEndpoints)
			udpPortMap := make(map[string]lbEndpoints)
			ovn.getLbEndpoints(&ep, tcpPortMap, udpPortMap)
			for svcPortName, lbEps := range tcpPortMap {
				ips := lbEps.IPs
				targetPort := lbEps.Port
				for _, svcPort := range svc.Spec.Ports {
					if svcPort.Protocol == kapi.ProtocolTCP && svcPort.Name == svcPortName {
						err = ovn.createLoadBalancerVIP(k8sNSLbTCP,
							physicalIP, svcPort.NodePort, ips, targetPort)
						if err != nil {
							logrus.Errorf("failed to create VIP in load balancer %s - %v", k8sNSLbTCP, err)
							continue
						}
					}
				}
			}
			for svcPortName, lbEps := range udpPortMap {
				ips := lbEps.IPs
				targetPort := lbEps.Port
				for _, svcPort := range svc.Spec.Ports {
					if svcPort.Protocol == kapi.ProtocolUDP && svcPort.Name == svcPortName {
						err = ovn.createLoadBalancerVIP(k8sNSLbUDP,
							physicalIP, svcPort.NodePort, ips, targetPort)
						if err != nil {
							logrus.Errorf("failed to create VIP in load balancer %s - %v", k8sNSLbUDP, err)
							continue
						}
					}
				}
			}
		}
	}
}

func (ovn *Controller) handleExternalIPs(svc *kapi.Service, svcPort kapi.ServicePort, ips []string, targetPort int32) {
	logrus.Debugf("handling external IPs for svc %v", svc.Name)
	if len(svc.Spec.ExternalIPs) == 0 {
		return
	}
	for _, extIP := range svc.Spec.ExternalIPs {
		lb := ovn.getDefaultGatewayLoadBalancer(svcPort.Protocol)
		if lb == "" {
			logrus.Warningf("No default gateway found for protocol %s\n\tNote: 'nodeport' flag needs to be enabled for default gateway", svcPort.Protocol)
			continue
		}
		err := ovn.createLoadBalancerVIP(lb, extIP, svcPort.Port, ips, targetPort)
		if err != nil {
			logrus.Errorf("Error in creating external IP for service: %s, externalIP: %s", svc.Name, extIP)
		}
	}
}

func (ovn *Controller) deleteEndpoints(ep *kapi.Endpoints) error {
	svc, err := ovn.kube.GetService(ep.Namespace, ep.Name)
	if err != nil {
		// This is not necessarily an error. For e.g when a service is deleted,
		// you will get endpoint delete event and the call to fetch service
		// will fail.
		logrus.Debugf("no service found for endpoint %s in namespace %s",
			ep.Name, ep.Namespace)
		return nil
	}
	if !util.IsClusterIPSet(svc) {
		return nil
	}
	for _, svcPort := range svc.Spec.Ports {
		var lb string
		lb, err = ovn.getLoadBalancer(svcPort.Protocol)
		if err != nil {
			logrus.Errorf("Failed to get load-balancer for %s (%v)",
				lb, err)
			continue
		}
		key := fmt.Sprintf("\"%s:%d\"", svc.Spec.ClusterIP, svcPort.Port)
		_, stderr, err := util.RunOVNNbctl("remove", "load_balancer", lb,
			"vips", key)
		if err != nil {
			logrus.Errorf("Error in deleting endpoints for lb %s, "+
				"stderr: %q (%v)", lb, stderr, err)
		}
	}
	return nil
}
