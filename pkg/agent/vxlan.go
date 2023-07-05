// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"

	"net"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/spidernet-io/egressgateway/pkg/agent/route"
	"github.com/spidernet-io/egressgateway/pkg/agent/vxlan"
	"github.com/spidernet-io/egressgateway/pkg/config"
	egressv1 "github.com/spidernet-io/egressgateway/pkg/k8s/apis/egressgateway.spidernet.io/v1beta1"
	"github.com/spidernet-io/egressgateway/pkg/utils"
)

type vxlanReconciler struct {
	client client.Client
	log    logr.Logger
	cfg    *config.Config

	peerMap *utils.SyncMap[string, vxlan.Peer]

	vxlan     *vxlan.Device
	getParent func(version int) (*vxlan.Parent, error)

	ruleRoute      *route.RuleRoute
	ruleRouteCache *utils.SyncMap[string, []net.IP]
}

type VTEP struct {
	IPv4 *net.IPNet
	IPv6 *net.IPNet
	MAC  net.HardwareAddr
}

func (r *vxlanReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	kind, newReq, err := utils.ParseKindWithReq(req)
	if err != nil {
		return reconcile.Result{}, err
	}
	log := r.log.WithValues("name", newReq.Name, "kind", kind)
	log.Info("reconciling")
	switch kind {
	case "EgressNode":
		return r.reconcileEgressNode(ctx, newReq, log)
	default:
		return reconcile.Result{}, nil
	}
}

// reconcileEgressNode
func (r *vxlanReconciler) reconcileEgressNode(ctx context.Context, req reconcile.Request, log logr.Logger) (reconcile.Result, error) {
	node := new(egressv1.EgressNode)
	deleted := false
	err := r.client.Get(ctx, req.NamespacedName, node)
	if err != nil {
		if !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		deleted = true
	}
	deleted = deleted || !node.GetDeletionTimestamp().IsZero()

	isPeer := true
	if req.Name == r.cfg.NodeName {
		isPeer = false
	}
	if deleted {
		if isPeer {
			r.peerMap.Delete(req.Name)
			err := r.ensureRoute()
			if err != nil {
				log.Error(err, "delete egress node, ensure route with error")
			}
		}
		return reconcile.Result{}, nil
	}

	// early check for early return
	if isPeer {
		var ip string
		if r.version() == 4 {
			ip = node.Status.Tunnel.Parent.IPv4
		} else {
			ip = node.Status.Tunnel.Parent.IPv6
		}
		if ip == "" {
			log.Info("parent ip not ready, skip", "peer", node.Name)
			return reconcile.Result{}, nil
		}

		parentIP := net.ParseIP(ip)
		mac, err := net.ParseMAC(node.Status.Tunnel.MAC)
		if err != nil {
			log.Info("mac addr not ready, skip", "mac", node.Status.Tunnel.MAC)
			return reconcile.Result{}, nil
		}

		ipv4 := net.ParseIP(node.Status.Tunnel.IPv4).To4()
		ipv6 := net.ParseIP(node.Status.Tunnel.IPv6).To16()

		peer := vxlan.Peer{Parent: parentIP, MAC: mac}
		if ipv4 != nil {
			peer.IPv4 = &ipv4
		}
		if ipv6 != nil {
			peer.IPv6 = &ipv6
		}
		baseMark, err := parseMarkToInt(node.Status.Mark)
		if err != nil {
		} else {
			peer.Mark = baseMark
		}

		r.peerMap.Store(node.Name, peer)
		err = r.ensureRoute()
		if err != nil {
			log.Error(err, "add egress node, ensure route with error")
		}

		err = r.ruleRoute.Ensure(r.cfg.FileConfig.VXLAN.Name, peer.IPv4, peer.IPv6, peer.Mark, peer.Mark)
		if err != nil {
			r.log.Error(err, "ensure vxlan link")
		}
		return reconcile.Result{}, nil
	}

	err = r.ensureEgressNodeStatus(node)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *vxlanReconciler) ensureEgressNodeStatus(node *egressv1.EgressNode) error {
	needUpdate := false

	if r.version() == 4 && node.Status.Tunnel.Parent.IPv4 == "" {
		needUpdate = true
	}

	if r.version() == 6 && node.Status.Tunnel.Parent.IPv6 == "" {
		needUpdate = true
	}

	if needUpdate {
		err := r.updateEgressNodeStatus(node, r.version())
		if err != nil {
			return err
		}
	}

	vtep := r.parseVTEP(node.Status)
	if vtep != nil {
		r.peerMap.Store(r.cfg.EnvConfig.NodeName, *vtep)
	}
	return nil
}

func (r *vxlanReconciler) updateEgressNodeStatus(node *egressv1.EgressNode, version int) error {
	parent, err := r.getParent(version)
	if err != nil {
		return err
	}

	if node == nil {
		node = new(egressv1.EgressNode)
		ctx := context.Background()
		err = r.client.Get(ctx, types.NamespacedName{Name: r.cfg.NodeName}, node)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil
			}
			return err
		}
	}

	needUpdate := false
	if node.Status.Tunnel.Parent.Name != parent.Name {
		needUpdate = true
		node.Status.Tunnel.Parent.Name = parent.Name
	}

	if version == 4 {
		if node.Status.Tunnel.Parent.IPv4 != parent.IP.String() {
			needUpdate = true
			node.Status.Tunnel.Parent.IPv4 = parent.IP.String()
		}
		if node.Status.Tunnel.Parent.IPv6 != "" {
			needUpdate = true
			node.Status.Tunnel.Parent.IPv6 = ""
		}
	} else {
		if node.Status.Tunnel.Parent.IPv6 != parent.IP.String() {
			needUpdate = true
			node.Status.Tunnel.Parent.IPv6 = parent.IP.String()
		}
		if node.Status.Tunnel.Parent.IPv4 != "" {
			needUpdate = true
			node.Status.Tunnel.Parent.IPv4 = ""
		}
	}

	// calculate whether the state has changed, update if the status changes.
	vtep := r.parseVTEP(node.Status)
	if vtep != nil {
		phase := egressv1.EgressNodeSucceeded
		if node.Status.Phase != phase {
			needUpdate = true
			node.Status.Phase = phase
		}
	}

	if needUpdate {
		r.log.Info("update node status",
			"phase", node.Status.Phase,
			"tunnelIPv4", node.Status.Tunnel.IPv4,
			"tunnelIPv6", node.Status.Tunnel.IPv6,
			"parentName", node.Status.Tunnel.Parent.Name,
			"parentIPv4", node.Status.Tunnel.Parent.IPv4,
			"parentIPv6", node.Status.Tunnel.Parent.IPv6,
		)
		ctx := context.Background()
		err = r.client.Status().Update(ctx, node)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *vxlanReconciler) parseVTEP(status egressv1.EgressNodeStatus) *vxlan.Peer {
	var ipv4 *net.IP
	var ipv6 *net.IP
	ready := true

	if r.cfg.FileConfig.EnableIPv4 {
		if status.Tunnel.IPv4 == "" {
			ready = false
		} else {
			ip := net.ParseIP(status.Tunnel.IPv4)
			if ip.To4() == nil {
				ready = false
			}
			ipv4 = &ip
		}
	}
	if r.cfg.FileConfig.EnableIPv6 {
		if status.Tunnel.IPv6 == "" {
			ready = false
		} else {
			ip := net.ParseIP(status.Tunnel.IPv6)
			if ip.To16() == nil {
				ready = false
			}
			ipv6 = &ip
		}
	}

	mac, err := net.ParseMAC(status.Tunnel.MAC)
	if err != nil {
		ready = false
	}

	if !ready {
		return nil
	}
	return &vxlan.Peer{
		IPv4: ipv4,
		IPv6: ipv6,
		MAC:  mac,
	}
}

func (r *vxlanReconciler) version() int {
	version := 4
	if !r.cfg.FileConfig.EnableIPv4 && r.cfg.FileConfig.EnableIPv6 {
		version = 6
	}
	return version
}

func (r *vxlanReconciler) keepVXLAN() {
	reduce := false
	for {
		vtep, ok := r.peerMap.Load(r.cfg.EnvConfig.NodeName)
		if !ok {
			r.log.V(1).Info("vtep not ready")
			time.Sleep(time.Second)
			continue
		}

		name := r.cfg.FileConfig.VXLAN.Name
		vni := r.cfg.FileConfig.VXLAN.ID
		port := r.cfg.FileConfig.VXLAN.Port
		mac := vtep.MAC
		disableChecksumOffload := r.cfg.FileConfig.VXLAN.DisableChecksumOffload

		var ipv4, ipv6 *net.IPNet
		if r.cfg.FileConfig.EnableIPv4 && vtep.IPv4.To4() != nil {
			ipv4 = &net.IPNet{
				IP:   vtep.IPv4.To4(),
				Mask: r.cfg.FileConfig.TunnelIPv4Net.Mask,
			}
		}
		if r.cfg.FileConfig.EnableIPv6 && vtep.IPv6.To16() != nil {
			ipv6 = &net.IPNet{
				IP:   vtep.IPv6.To16(),
				Mask: r.cfg.FileConfig.TunnelIPv6Net.Mask,
			}
		}

		err := r.updateEgressNodeStatus(nil, r.version())
		if err != nil {
			r.log.Error(err, "update EgressNode status")
			time.Sleep(time.Second)
			continue
		}

		err = r.vxlan.EnsureLink(name, vni, port, mac, 0, ipv4, ipv6, disableChecksumOffload)
		if err != nil {
			r.log.Error(err, "ensure vxlan link")
			reduce = false
			time.Sleep(time.Second)
			continue
		}

		r.log.V(1).Info("link ensure has completed")

		err = r.ensureRoute()
		if err != nil {
			r.log.Error(err, "ensure route")
			reduce = false
			time.Sleep(time.Second)
			continue
		}

		r.log.V(1).Info("route ensure has completed")

		markMap := make(map[int]struct{})
		r.peerMap.Range(func(key string, val vxlan.Peer) bool {
			if val.Mark != 0 {
				markMap[val.Mark] = struct{}{}
			}
			err = r.ruleRoute.Ensure(r.cfg.FileConfig.VXLAN.Name, val.IPv4, val.IPv6, val.Mark, val.Mark)
			if err != nil {
				r.log.Error(err, "ensure vxlan link with error")
				reduce = false
			}
			return true
		})
		err = r.ruleRoute.PurgeStaleRules(markMap, r.cfg.FileConfig.Mark)
		if err != nil {
			r.log.Error(err, "purge stale rules error")
			reduce = false
		}

		r.log.V(1).Info("route rule ensure has completed")

		if !reduce {
			r.log.Info("vxlan and route has completed")
			reduce = true
		}

		time.Sleep(time.Second * 10)
	}
}

func (r *vxlanReconciler) ensureRoute() error {
	neighList, err := r.vxlan.ListNeigh()
	if err != nil {
		return err
	}

	peerMap := make(map[string]vxlan.Peer, 0)
	r.peerMap.Range(func(key string, peer vxlan.Peer) bool {
		if key == r.cfg.EnvConfig.NodeName {
			return true
		}
		peerMap[key] = peer
		return true
	})

	expected := make(map[string]struct{}, 0)
	for _, peer := range peerMap {
		expected[peer.MAC.String()] = struct{}{}
	}

	for _, item := range neighList {
		if _, ok := expected[item.HardwareAddr.String()]; !ok {
			err := r.vxlan.Del(item)
			if err != nil {
				r.log.Error(err, "delete link layer neighbor", "item", item.String())
			}
		}
	}

	for _, peer := range peerMap {
		err := r.vxlan.Add(peer)
		if err != nil {
			r.log.Error(err, "add peer route", "peer", peer)
		}
	}

	return nil
}

func newEgressNodeController(mgr manager.Manager, cfg *config.Config, log logr.Logger) error {
	ruleRoute := route.NewRuleRoute(log)

	r := &vxlanReconciler{
		client:         mgr.GetClient(),
		log:            log,
		cfg:            cfg,
		peerMap:        utils.NewSyncMap[string, vxlan.Peer](),
		vxlan:          vxlan.New(),
		ruleRoute:      ruleRoute,
		ruleRouteCache: utils.NewSyncMap[string, []net.IP](),
	}
	netLink := vxlan.NetLink{
		RouteListFiltered: netlink.RouteListFiltered,
		LinkByIndex:       netlink.LinkByIndex,
		AddrList:          netlink.AddrList,
		LinkByName:        netlink.LinkByName,
	}
	if strings.HasPrefix(cfg.FileConfig.TunnelDetectMethod, config.TunnelInterfaceSpecific) {
		name := strings.TrimPrefix(cfg.FileConfig.TunnelDetectMethod, config.TunnelInterfaceSpecific)
		r.getParent = vxlan.GetParentByName(netLink, name)
	} else {
		r.getParent = vxlan.GetParentByDefaultRoute(netLink)
	}

	c, err := controller.New("vxlan", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	if err := c.Watch(source.Kind(mgr.GetCache(), &egressv1.EgressNode{}),
		handler.EnqueueRequestsFromMapFunc(utils.KindToMapFlat("EgressNode"))); err != nil {
		return fmt.Errorf("failed to watch EgressNode: %w", err)
	}

	go r.keepVXLAN()

	return nil
}
