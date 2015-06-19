/*
 * Cherry - An OpenFlow Controller
 *
 * Copyright (C) 2015 Samjung Data Service Co., Ltd.,
 * Kitae Kim <superkkt@sds.co.kr>
 */

package network

import (
	"encoding"
	"git.sds.co.kr/cherry.git/cherryd/internal/log"
	"git.sds.co.kr/cherry.git/cherryd/openflow"
	"sync"
)

type Descriptions struct {
	Manufacturer string
	Hardware     string
	Software     string
	Serial       string
	Description  string
}

type Features struct {
	DPID       uint64
	NumBuffers uint32
	NumTables  uint8
}

type Device struct {
	mutex        sync.RWMutex
	id           string
	log          log.Logger
	session      *session
	descriptions Descriptions
	features     Features
	ports        map[uint32]*Port
	flowTableID  uint8 // Table IDs that we install flows
	factory      openflow.Factory
}

func newDevice(log log.Logger, s *session) *Device {
	if log == nil {
		panic("Logger is nil")
	}
	if s == nil {
		panic("Session is nil")
	}

	return &Device{
		log:     log,
		session: s,
		ports:   make(map[uint32]*Port),
	}
}

func (r *Device) ID() string {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return r.id
}

func (r *Device) setID(id string) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.id = id
}

func (r *Device) isValid() bool {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return len(r.id) > 0
}

func (r *Device) Factory() openflow.Factory {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return r.factory
}

func (r *Device) setFactory(f openflow.Factory) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if f == nil {
		panic("Factory is nil")
	}
	r.factory = f
}

func (r *Device) Descriptions() Descriptions {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return r.descriptions
}

func (r *Device) setDescriptions(d Descriptions) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.descriptions = d
}

func (r *Device) Features() Features {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return r.features
}

func (r *Device) setFeatures(f Features) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.features = f
}

// Port may return nil if there is no port whose number is num
func (r *Device) Port(num uint32) *Port {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return r.ports[num]
}

func (r *Device) Ports() []*Port {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	p := make([]*Port, 0)
	for _, v := range r.ports {
		p = append(p, v)
	}

	return p
}

// A caller should make sure the mutex is locked before calling this function
func (r *Device) setPort(num uint32, p openflow.Port) {
	port := NewPort(r, num)
	port.SetValue(p)
	r.ports[num] = port
}

func (r *Device) addPort(num uint32, p openflow.Port) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if p == nil {
		panic("Port is nil")
	}
	r.setPort(num, p)
}

func (r *Device) updatePort(num uint32, p openflow.Port) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if p == nil {
		panic("Port is nil")
	}
	port := r.ports[num]
	if port == nil {
		r.setPort(num, p)
	} else {
		port.SetValue(p)
	}
}

func (r *Device) FlowTableID() uint8 {
	// Read lock
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return r.flowTableID
}

func (r *Device) setFlowTableID(id uint8) {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.flowTableID = id
}

func (r *Device) SendMessage(msg encoding.BinaryMarshaler) error {
	// Write lock
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if msg == nil {
		panic("Message is nil")
	}

	return r.session.Write(msg)
}