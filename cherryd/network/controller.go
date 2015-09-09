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

package network

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/ant0ine/go-json-rest/rest"
	"github.com/dlintw/goconf"
	"github.com/superkkt/cherry/cherryd/log"
	"github.com/superkkt/cherry/cherryd/protocol"
	"golang.org/x/net/context"
)

type database interface {
	AddSwitch(Switch) error
	Location(mac net.HardwareAddr) (dpid string, port uint32, ok bool, err error)
	RemoveSwitch(id uint64) (ok bool, err error)
	Switch(dpid uint64) (sw RegisteredSwitch, ok bool, err error)
	Switches() ([]RegisteredSwitch, error)
}

type EventListener interface {
	ControllerEventListener
	TopologyEventListener
}

type ControllerEventListener interface {
	OnPacketIn(Finder, *Port, *protocol.Ethernet) error
	OnPortUp(Finder, *Port) error
	OnPortDown(Finder, *Port) error
	OnDeviceUp(Finder, *Device) error
	OnDeviceDown(Finder, *Device) error
}

type TopologyEventListener interface {
	OnTopologyChange(Finder) error
}

type Controller struct {
	log      log.Logger
	topo     *topology
	listener EventListener
	db       database
}

func NewController(log log.Logger, db database, conf *goconf.ConfigFile) *Controller {
	if log == nil {
		panic("Logger is nil")
	}

	v := &Controller{
		log:  log,
		topo: newTopology(log, db),
		db:   db,
	}
	go v.serveREST(conf)

	return v
}

func (r *Controller) serveREST(conf *goconf.ConfigFile) {
	c, err := parseRESTConfig(conf)
	if err != nil {
		r.log.Err(fmt.Sprintf("Controller: parsing REST configurations: %v", err))
		return
	}

	api := rest.NewApi()
	router, err := rest.MakeRouter(
		rest.Get("/api/v1/switch", r.listSwitch),
		rest.Post("/api/v1/switch", r.addSwitch),
		rest.Delete("/api/v1/switch/:id", r.removeSwitch),
	)
	if err != nil {
		r.log.Err(fmt.Sprintf("Controller: making a REST router: %v", err))
		return
	}
	api.SetApp(router)

	addr := fmt.Sprintf(":%v", c.port)
	if c.tls.enable {
		err = http.ListenAndServeTLS(addr, c.tls.certFile, c.tls.keyFile, api.MakeHandler())
	} else {
		err = http.ListenAndServe(addr, api.MakeHandler())
	}

	if err != nil {
		r.log.Err(fmt.Sprintf("Controller: listening on HTTP: %v", err))
		return
	}
}

type restConfig struct {
	port uint16
	tls  struct {
		enable   bool
		certFile string
		keyFile  string
	}
}

func parseRESTConfig(conf *goconf.ConfigFile) (*restConfig, error) {
	var err error
	c := &restConfig{}

	c.tls.enable, err = conf.GetBool("rest", "tls")
	if err != nil {
		return nil, errors.New("invalid rest/tls value")
	}

	port, err := conf.GetInt("rest", "port")
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("empty or invalid rest/port value")
	}
	c.port = uint16(port)

	c.tls.certFile, err = conf.GetString("rest", "cert_file")
	if err != nil || len(c.tls.certFile) == 0 {
		return nil, errors.New("empty rest/cert_file value")
	}
	if c.tls.certFile[0] != '/' {
		return nil, errors.New("rest/cert_file should be specified as an absolute path")
	}

	c.tls.keyFile, err = conf.GetString("rest", "key_file")
	if err != nil || len(c.tls.keyFile) == 0 {
		return nil, errors.New("empty rest/key_file value")
	}
	if c.tls.keyFile[0] != '/' {
		return nil, errors.New("rest/key_file should be specified as an absolute path")
	}

	return c, nil
}

type Switch struct {
	DPID        uint64 `json:"dpid"`
	NumPorts    uint16 `json:"n_ports"`
	FirstPort   uint16 `json:"first_port"`
	Description string `json:"description"`
}

func (r *Switch) validate() error {
	if r.NumPorts > 512 {
		return errors.New("too many ports")
	}
	if uint32(r.FirstPort)+uint32(r.NumPorts) > 0xFFFF {
		return errors.New("too high first port number")
	}

	return nil
}

type RegisteredSwitch struct {
	ID uint64 `json:"id"`
	Switch
}

func (r *Controller) listSwitch(w rest.ResponseWriter, req *rest.Request) {
	sw, err := r.db.Switches()
	if err != nil {
		writeStatus(w, queryFailed, err)
		return
	}

	w.WriteJson(&struct {
		Status   int                `json:"status"`
		Msg      string             `json:"msg"`
		Switches []RegisteredSwitch `json:"switches"`
	}{okay, statusMsgs[okay], sw})
}

func (r *Controller) addSwitch(w rest.ResponseWriter, req *rest.Request) {
	sw := Switch{}
	if err := req.DecodeJsonPayload(&sw); err != nil {
		writeStatus(w, decodeFailed, err)
		return
	}
	if err := sw.validate(); err != nil {
		writeStatus(w, invalidParam, err)
		return
	}
	_, ok, err := r.db.Switch(sw.DPID)
	if err != nil {
		writeStatus(w, queryFailed, err)
		return
	}
	if ok {
		writeStatus(w, duplicatedDPID)
		return
	}
	if err := r.db.AddSwitch(sw); err != nil {
		writeStatus(w, queryFailed, err)
		return
	}
	writeStatus(w, okay)
}

func (r *Controller) removeSwitch(w rest.ResponseWriter, req *rest.Request) {
	id, err := strconv.ParseUint(req.PathParam("id"), 10, 64)
	if err != nil {
		writeStatus(w, invalidSwitchID, err)
		return
	}

	ok, err := r.db.RemoveSwitch(id)
	if err != nil {
		writeStatus(w, queryFailed, err)
		return
	}
	if !ok {
		writeStatus(w, unknownSwitchID)
		return
	}

	for _, sw := range r.topo.Devices() {
		if err := sw.RemoveAllFlows(); err != nil {
			r.log.Warning(fmt.Sprintf("Controller: failed to remove all flows on %v device: %v", sw.ID(), err))
			continue
		}
	}

	writeStatus(w, okay)
}

const (
	okay = iota
	queryFailed
	decodeFailed
	invalidParam
	duplicatedDPID
	invalidSwitchID
	unknownSwitchID
	internalServerErr
)

var statusMsgs = map[int]string{
	okay:              "no error",
	queryFailed:       "failed to query from database: %v",
	decodeFailed:      "failed to decode input parameters: %v",
	invalidParam:      "invalid input parameter: %v",
	duplicatedDPID:    "duplicated switch DPID",
	invalidSwitchID:   "invalid switch ID: %v",
	unknownSwitchID:   "unknown switch ID",
	internalServerErr: "internal server error",
}

func writeStatus(w rest.ResponseWriter, status int, args ...interface{}) {
	format, ok := statusMsgs[status]
	if !ok {
		panic(fmt.Sprintf("unknown status code: %v", status))
	}

	w.WriteJson(struct {
		Status int    `json:"status"`
		Msg    string `json:"msg"`
	}{status, fmt.Sprintf(format, args...)})
}

func (r *Controller) AddConnection(ctx context.Context, c net.Conn) {
	conf := sessionConfig{
		conn:     c,
		logger:   r.log,
		watcher:  r.topo,
		finder:   r.topo,
		listener: r.listener,
	}
	session := newSession(conf)
	go session.Run(ctx)
}

func (r *Controller) SetEventListener(l EventListener) {
	r.listener = l
	r.topo.setEventListener(l)
}

func (r *Controller) String() string {
	return r.topo.String()
}
