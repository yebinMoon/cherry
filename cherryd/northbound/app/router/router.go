/*
 * Cherry - An OpenFlow Controller
 *
 * Copyright (C) 2015 Samjung Data Service, Inc. All rights reserved.
 * Kitae Kim <superkkt@sds.co.kr>
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License along
 * with this program; if not, write to the Free Software Foundation, Inc.,
 * 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301 USA.
 */

package router

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/dlintw/goconf"
	"github.com/superkkt/cherry/cherryd/log"
	"github.com/superkkt/cherry/cherryd/network"
	"github.com/superkkt/cherry/cherryd/northbound/app"
	"github.com/superkkt/cherry/cherryd/openflow"
	"github.com/superkkt/cherry/cherryd/protocol"
	"math/rand"
	"net"
)

// TODO: Implement Border Gateway Protocol (BGP) to directly communicate with external routers
type Router struct {
	app.BaseProcessor
	conf *goconf.ConfigFile
	log  log.Logger
	db   database
	// Virtual MAC address
	mac net.HardwareAddr
}

type database interface {
	FindMAC(ip net.IP) (mac net.HardwareAddr, ok bool, err error)
	GetGateways() ([]net.HardwareAddr, error)
	GetNetworks() ([]*net.IPNet, error)
	IsGateway(mac net.HardwareAddr) (bool, error)
	IsRouter(ip net.IP) (bool, error)
}

func New(conf *goconf.ConfigFile, log log.Logger, db database) *Router {
	return &Router{
		conf: conf,
		log:  log,
		db:   db,
	}
}

func (r *Router) Name() string {
	return "Router"
}

func (r *Router) Init() error {
	mac, err := r.conf.GetString("router", "mac")
	if err != nil || len(mac) == 0 {
		return errors.New("empty virtual MAC address of the router in the config file")
	}
	r.mac, err = net.ParseMAC(mac)
	if err != nil {
		return err
	}

	return nil
}

func (r *Router) OnPacketIn(finder network.Finder, ingress *network.Port, eth *protocol.Ethernet) error {
	r.log.Debug(fmt.Sprintf("Router: PACKET_IN.. Ingress=%v, SrcMAC=%v, DstMAC=%v", ingress.ID(), eth.SrcMAC, eth.DstMAC))

	// Is this packet going to the router?
	if bytes.Compare(eth.DstMAC, r.mac) != 0 {
		r.log.Debug(fmt.Sprintf("Router: ignore PACKET_IN as it's not going to the router.. Ingress=%v, SrcMAC=%v, DstMAC=%v", ingress.ID(), eth.SrcMAC, eth.DstMAC))
		return r.BaseProcessor.OnPacketIn(finder, ingress, eth)
	}

	// IPv4?
	if eth.Type != 0x0800 {
		r.log.Debug(fmt.Sprintf("Router: drop non-IPv4 packet.. (ethType=%v, SrcMAC=%v, DstMAC=%v)", eth.Type, eth.SrcMAC, eth.DstMAC))
		// Drop the packet if it is not an IPv4 packet
		return nil
	}
	ipv4 := new(protocol.IPv4)
	if err := ipv4.UnmarshalBinary(eth.Payload); err != nil {
		return err
	}
	p := packet{
		ingress:  ingress,
		ethernet: eth,
		ipv4:     ipv4,
	}

	ok, err := r.db.IsRouter(ipv4.DstIP)
	if err != nil {
		return fmt.Errorf("checking router IP: %v", err)
	}
	if ok {
		// ICMP?
		if ipv4.Protocol == 0x01 {
			return r.sendICMPReply(p)
		}
		return nil
	}

	mine, err := r.isMyNetwork(ipv4.DstIP)
	if err != nil {
		return fmt.Errorf("checking my networks: %v", err)
	}
	if mine {
		return r.handleIncoming(finder, p)
	} else {
		return r.handleOutgoing(finder, p)
	}
}

func (r *Router) sendICMPReply(p packet) error {
	echo := new(protocol.ICMPEcho)
	if err := echo.UnmarshalBinary(p.ipv4.Payload); err != nil || echo.Type != 8 {
		// Ignores all other ICMP messages except ICMP echo reqeust
		r.log.Debug("Rotuer: ignores ICMP message that is not echo request..")
		return nil
	}

	reply := protocol.NewICMPEchoReply(echo.ID, echo.Sequence, echo.Payload)
	replyPacket, err := reply.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshaling ICMP echo reply: %v", err)
	}

	ip := protocol.NewIPv4(p.ipv4.DstIP, p.ipv4.SrcIP, 0x01, replyPacket)
	ipPacket, err := ip.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshaling IPv4 packet: %v", err)
	}

	eth := protocol.Ethernet{
		SrcMAC:  p.ethernet.DstMAC,
		DstMAC:  p.ethernet.SrcMAC,
		Type:    0x0800,
		Payload: ipPacket,
	}
	ethPacket, err := eth.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshaling ethernet packet: %v", err)
	}

	r.log.Debug(fmt.Sprintf("Router: sending ICMP reply to %v..", p.ipv4.SrcIP))
	return r.PacketOut(p.ingress, ethPacket)
}

func (r *Router) isMyNetwork(ip net.IP) (bool, error) {
	networks, err := r.db.GetNetworks()
	if err != nil {
		return false, err
	}

	for _, n := range networks {
		if n.Contains(ip) {
			return true, nil
		}
	}

	return false, nil
}

type packet struct {
	ingress  *network.Port
	ethernet *protocol.Ethernet
	ipv4     *protocol.IPv4
}

func (r *Router) handleIncoming(finder network.Finder, p packet) error {
	r.log.Debug(fmt.Sprintf("Router: handle incoming packet.. SrcMAC=%v, DstMAC=%v, SrcIP=%v, DstIP=%v", p.ethernet.SrcMAC, p.ethernet.DstMAC, p.ipv4.SrcIP, p.ipv4.DstIP))

	mac, ok, err := r.db.FindMAC(p.ipv4.DstIP)
	if err != nil {
		return err
	}
	if !ok {
		r.log.Debug(fmt.Sprintf("Router: drop the incoming packet that goes to an unknown host %v from %v", p.ipv4.DstIP, p.ipv4.SrcIP))
		return nil
	}
	r.log.Debug(fmt.Sprintf("Router: routing the incoming packet to a host.. DstIP=%v, DstMAC=%v", p.ipv4.DstIP, mac))

	return r.route(finder, p, mac)
}

// XXX: We only support static default routing
func (r *Router) handleOutgoing(finder network.Finder, p packet) error {
	r.log.Debug(fmt.Sprintf("Router: handle outgoing packet.. SrcMAC=%v, DstMAC=%v, SrcIP=%v, DstIP=%v", p.ethernet.SrcMAC, p.ethernet.DstMAC, p.ipv4.SrcIP, p.ipv4.DstIP))

	r.log.Debug(fmt.Sprintf("Router: checking whether the outgoing packet (SrcMAC=%v) came from a gateway..", p.ethernet.SrcMAC))
	ok, err := r.db.IsGateway(p.ethernet.SrcMAC)
	if err != nil {
		return fmt.Errorf("checking gateway MAC: %v", err)
	}
	if ok {
		r.log.Err(fmt.Sprintf("Router: Loop is detected!! Did you add network address for %v?", p.ipv4.DstIP))
		// Drop this packet
		return nil
	}

	mine, err := r.isMyNetwork(p.ipv4.SrcIP)
	if err != nil {
		return fmt.Errorf("checking my networks: %v", err)
	}
	// IP spoofing?
	if !mine {
		r.log.Warning(fmt.Sprintf("Router: IP spoofing is detected!! SrcIP=%v, DstIP=%v", p.ipv4.SrcIP, p.ipv4.DstIP))
		// Drop this packet
		return nil
	}

	gateways, err := r.db.GetGateways()
	if err != nil {
		return fmt.Errorf("query gateway MAC addresses: %v", err)
	}
	if gateways == nil || len(gateways) == 0 {
		r.log.Err("Router: not found a gateway MAC address for outgoing packets!")
		// Drop this packet
		return nil
	}
	for _, v := range gateways {
		r.log.Debug(fmt.Sprintf("Router: found a Gateway MAC: %v", v))
	}
	mac := pickGateway(gateways)
	r.log.Debug(fmt.Sprintf("Router: gateway %v is selected for the outgoing packet!", mac))

	return r.route(finder, p, mac)
}

func (r *Router) route(finder network.Finder, p packet, mac net.HardwareAddr) error {
	// Do we have the destination node?
	dstNode := finder.Node(mac)
	if dstNode == nil {
		r.log.Debug(fmt.Sprintf("Router: we don't know where the node is connected.. (MAC=%v)", mac))
		// Replace destination MAC address
		p.ethernet.DstMAC = mac
		return r.BaseProcessor.OnPacketIn(finder, p.ingress, p.ethernet)
	}

	// Two nodes on a same switch device?
	if p.ingress.Device().ID() == dstNode.Port().Device().ID() {
		r.log.Debug("Router: two nodes are connected to a same switch..")
		return r.sendPacket(p, dstNode.Port(), mac)
	}
	path := finder.Path(p.ingress.Device().ID(), dstNode.Port().Device().ID())
	if path == nil || len(path) == 0 {
		r.log.Info(fmt.Sprintf("Router: not found a path from %v to %v", p.ingress.ID(), dstNode.Port().ID()))
		return nil
	}
	return r.sendPacket(p, path[0][0], mac)
}

func (r *Router) sendPacket(p packet, egress *network.Port, mac net.HardwareAddr) error {
	// Make a flow parameter using unmodified destination MAC address
	param := flowParam{
		device:    p.ingress.Device(),
		etherType: p.ethernet.Type,
		inPort:    p.ingress.Number(),
		outPort:   egress.Number(),
		srcMAC:    p.ethernet.SrcMAC,
		dstMAC:    p.ethernet.DstMAC,
		targetMAC: mac,
		dstIP:     &net.IPNet{IP: p.ipv4.DstIP, Mask: net.IPv4Mask(255, 255, 255, 255)},
	}

	// Replace destination MAC address
	p.ethernet.DstMAC = mac
	packet, err := p.ethernet.MarshalBinary()
	if err != nil {
		return err
	}
	// Install a flow rule that replaces the destination MAC address
	r.log.Debug(fmt.Sprintf("Router: installing a flow.. %+v", param))
	if err := installFlow(param); err != nil {
		return err
	}

	r.log.Debug(fmt.Sprintf("Router: sending a packet to egress port %v..", egress.ID()))
	return r.PacketOut(egress, packet)
}

func pickGateway(gateways []net.HardwareAddr) net.HardwareAddr {
	if gateways == nil || len(gateways) == 0 {
		panic("Invalid gateways")
	}

	if len(gateways) == 1 {
		return gateways[0]
	}
	return gateways[rand.Intn(len(gateways))]
}

type flowParam struct {
	device    *network.Device
	etherType uint16
	inPort    uint32
	outPort   uint32
	srcMAC    net.HardwareAddr
	dstMAC    net.HardwareAddr
	targetMAC net.HardwareAddr
	dstIP     *net.IPNet
}

func installFlow(p flowParam) error {
	f := p.device.Factory()

	inPort := openflow.NewInPort()
	inPort.SetValue(p.inPort)
	match, err := f.NewMatch()
	if err != nil {
		return err
	}
	match.SetInPort(inPort)
	match.SetEtherType(p.etherType)
	match.SetSrcMAC(p.srcMAC)
	match.SetDstMAC(p.dstMAC)
	match.SetDstIP(p.dstIP)

	outPort := openflow.NewOutPort()
	outPort.SetValue(p.outPort)
	action, err := f.NewAction()
	if err != nil {
		return err
	}
	action.SetDstMAC(p.targetMAC)
	action.SetOutPort(outPort)
	inst, err := f.NewInstruction()
	if err != nil {
		return err
	}
	inst.ApplyAction(action)

	flow, err := f.NewFlowMod(openflow.FlowAdd)
	if err != nil {
		return err
	}
	flow.SetTableID(p.device.FlowTableID())
	flow.SetIdleTimeout(30)
	// This priority should be higher than L2 switch module's one
	flow.SetPriority(30)
	flow.SetFlowMatch(match)
	flow.SetFlowInstruction(inst)

	return p.device.SendMessage(flow)
}