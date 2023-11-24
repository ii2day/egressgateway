// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

// This code is copied from the metallb project, which is also licensed under
// the Apache License, Version 2.0. The original code can be found at:
// https://github.com/metallb/metallb
// SPDX-License-Identifier:Apache-2.0

package layer2

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/go-logr/logr"
	"github.com/mdlayher/ndp"
)

type ndpResponder struct {
	logger       logr.Logger
	intf         string
	hardwareAddr net.HardwareAddr
	conn         *ndp.Conn
	closed       chan struct{}
	announce     announceFunc
	// Refcount of how many watchers for each solicited node
	// multicast group.
	solicitedNodeGroups map[string]int64
}

func newNDPResponder(logger logr.Logger, ifi *net.Interface, ann announceFunc) (*ndpResponder, error) {
	// Use link-local address as the source IPv6 address for NDP communications.
	conn, _, err := ndp.Dial(ifi, ndp.LinkLocal)
	if err != nil {
		return nil, fmt.Errorf("creating NDP responder for %q: %s", ifi.Name, err)
	}

	ret := &ndpResponder{
		logger:              logger,
		intf:                ifi.Name,
		hardwareAddr:        ifi.HardwareAddr,
		conn:                conn,
		closed:              make(chan struct{}),
		announce:            ann,
		solicitedNodeGroups: map[string]int64{},
	}
	go ret.run()
	return ret, nil
}

func (n *ndpResponder) Interface() string { return n.intf }

func (n *ndpResponder) Close() error {
	close(n.closed)
	return n.conn.Close()
}

func (n *ndpResponder) Gratuitous(ip net.IP) error {
	err := n.advertise(net.IPv6linklocalallnodes, ip, true)
	stats.SentGratuitous(ip.String())
	return err
}

func (n *ndpResponder) Watch(ip net.IP) error {
	if ip.To4() != nil {
		return nil
	}
	group, err := ndp.SolicitedNodeMulticast(ip)
	if err != nil {
		return fmt.Errorf("looking up solicited node multicast group for %q: %s", ip, err)
	}
	if n.solicitedNodeGroups[group.String()] == 0 {
		if err = n.conn.JoinGroup(group); err != nil {
			return fmt.Errorf("joining solicited node multicast group for %q: %s", ip, err)
		}
	}
	n.solicitedNodeGroups[group.String()]++
	return nil
}

func (n *ndpResponder) Unwatch(ip net.IP) error {
	if ip.To4() != nil {
		return nil
	}
	group, err := ndp.SolicitedNodeMulticast(ip)
	if err != nil {
		return fmt.Errorf("looking up solicited node multicast group for %q: %s", ip, err)
	}
	n.solicitedNodeGroups[group.String()]--
	if n.solicitedNodeGroups[group.String()] == 0 {
		if err = n.conn.LeaveGroup(group); err != nil {
			return fmt.Errorf("leaving solicited node multicast group for %q: %s", ip, err)
		}
	}
	return nil
}

func (n *ndpResponder) run() {
	for n.processRequest() != dropReasonClosed {
	}
}

func (n *ndpResponder) processRequest() dropReason {
	msg, _, src, err := n.conn.ReadFrom()
	if err != nil {
		select {
		case <-n.closed:
			return dropReasonClosed
		default:
		}
		if errors.Is(err, io.EOF) {
			return dropReasonClosed
		}
		return dropReasonError
	}

	ns, ok := msg.(*ndp.NeighborSolicitation)
	if !ok {
		return dropReasonMessageType
	}

	// Retrieve sender's source link-layer address
	var nsLLAddr net.HardwareAddr
	for _, o := range ns.Options {
		// Ignore other options, including target link-layer address instead of source.
		lla, ok := o.(*ndp.LinkLayerAddress)
		if !ok {
			continue
		}
		if lla.Direction != ndp.Source {
			continue
		}

		nsLLAddr = lla.Addr
		break
	}
	if nsLLAddr == nil {
		return dropReasonNoSourceLL
	}

	// Ignore NDP requests that the announcer tells us to ignore.
	reason := n.announce(ns.TargetAddress, n.intf)
	if reason == dropReasonNotMatchInterface {
		n.logger.V(1).Info("ignore NDP requests", "op", "ndpRequestIgnore", "ip", ns.TargetAddress, "interface", n.intf, "reason", "notMatchInterface")
	}
	if reason != dropReasonNone {
		return reason
	}

	stats.GotRequest(ns.TargetAddress.String())
	n.logger.V(1).Info("got NDP request for service IP, sending response", "interface", n.intf, "ip", ns.TargetAddress, "senderIP", src, "senderLLAddr", nsLLAddr, "responseMAC", n.hardwareAddr)
	if err := n.advertise(src, ns.TargetAddress, false); err != nil {
		n.logger.Error(err, "failed to send ARP reply", "op", "ndpReply", "interface", n.intf, "ip", ns.TargetAddress, "senderIP", src, "senderLLAddr", nsLLAddr, "responseMAC", n.hardwareAddr)
	} else {
		stats.SentResponse(ns.TargetAddress.String())
	}
	return dropReasonNone
}

func (n *ndpResponder) advertise(dst, target net.IP, gratuitous bool) error {
	m := &ndp.NeighborAdvertisement{
		Solicited:     !gratuitous, // <Adam Jensen> I never asked for this...
		Override:      gratuitous,  // Should clients replace existing cache entries
		TargetAddress: target,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Target,
				Addr:      n.hardwareAddr,
			},
		},
	}
	return n.conn.WriteTo(m, nil, dst)
}
