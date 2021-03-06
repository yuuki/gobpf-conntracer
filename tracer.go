// +build linux

package conntracer

import (
	"errors"
	"fmt"
	"log"
	"net"
	"syscall"
	"time"
	"unsafe"

	// Put the C header files into Go module management
	_ "github.com/yuuki/go-conntracer-bpf/include"
	_ "github.com/yuuki/go-conntracer-bpf/include/bpf"
	"golang.org/x/sync/errgroup"
)

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lelf -lz

#include <sys/resource.h>
#include <arpa/inet.h>
#include <errno.h>

#include <bpf/libbpf.h>
#include <bpf/bpf.h>
#include "conntracer.skel.h"
#include "conntracer.h"

*/
import "C"

// FlowDirection are bitmask that represents both Active or Passive.
type FlowDirection uint8

const (
	// FlowUnknown are unknown flow.
	FlowUnknown FlowDirection = iota + 1
	// FlowActive are 'active open'.
	FlowActive
	// FlowPassive are 'passive open'
	FlowPassive

	// defaultFlowMapOpsBatchSize is batch size of BPF map(flows) lookup_and_delete.
	defaultFlowMapOpsBatchSize = 10
)

func flowDirectionFrom(x C.flow_direction) FlowDirection {
	switch x {
	case C.FLOW_UNKNOWN:
		return FlowUnknown
	case C.FLOW_ACTIVE:
		return FlowActive
	case C.FLOW_PASSIVE:
		return FlowPassive
	}
	return FlowUnknown
}

/*
aggregated_flow_tuple
__u32 saddr;
__u32 daddr;
__u16 lport;
__u8 direction;
__u8 l4_proto;
*/
type AggrFlowTuple C.struct_aggregated_flow_tuple

// Flow is a bunch of aggregated flows grouped by listening port.
type Flow struct {
	SAddr       *net.IP
	DAddr       *net.IP
	ProcessName string
	LPort       uint16 // Listening port
	Direction   FlowDirection
	LastPID     uint32
	L4Proto     uint8
	Stat        *AggrFlowStat
}

// AggrFlowStat is an statistics for aggregated flows.
type AggrFlowStat struct {
	Timestamp time.Time
	sentBytes uint64
	recvBytes uint64
}

// SentBytes returns sent kB/sec.
func (s *AggrFlowStat) SentBytes(d time.Duration) float64 {
	return float64(s.sentBytes) / 1024 / d.Seconds()
}

// RecvBytes returns recv kB/sec.
func (s *AggrFlowStat) RecvBytes(d time.Duration) float64 {
	return float64(s.recvBytes) / 1024 / d.Seconds()
}

/*
flow_tuple
__u32 saddr;
__u32 daddr;
__u16 sport;
__u16 dport;
__u32 pid;
__u8 l4_proto;
*/
type SingleFlowTuple C.struct_flow_tuple

// SingleFlow is a single flow.
type SingleFlow struct {
	SAddr       *net.IP
	DAddr       *net.IP
	SPort       uint16
	DPort       uint16
	LPort       uint16
	Direction   FlowDirection
	PID         uint32
	ProcessName string
	L4Proto     uint8
	Stat        *SingleFlowStat
}

// SingleFlowStat is an statistics for single flow.
type SingleFlowStat struct {
	Timestamp time.Time
	sentBytes uint64
	recvBytes uint64
}

// SentBytes returns sent kB/sec.
func (s *SingleFlowStat) SentBytes(d time.Duration) float64 {
	return float64(s.sentBytes) / 1024 / d.Seconds()
}

// RecvBytes returns recv kB/sec.
func (s *SingleFlowStat) RecvBytes(d time.Duration) float64 {
	return float64(s.recvBytes) / 1024 / d.Seconds()
}

// FlowStat is an statistics for Flow.
type FlowStat struct {
	NewConnections uint32
	SentBytes      uint64
	RecvBytes      uint64
}

// Tracer is an object for state retention.
type Tracer struct {
	obj      *C.struct_conntracer_bpf
	stopChan chan struct{}
	statsFd  int

	// option
	batchSize int
}

// TracerParam is a parameter for NewTracer.
type TracerParam struct {
	Stats bool
}

// NewTracer creates a Tracer object.
func NewTracer(param *TracerParam) (*Tracer, error) {
	// Bump RLIMIT_MEMLOCK to allow BPF sub-system to do anything
	if err := bumpMemlockRlimit(); err != nil {
		return nil, err
	}

	obj := C.conntracer_bpf__open_and_load()
	if obj == nil {
		return nil, errors.New("failed to open and load BPF object")
	}

	cerr := C.conntracer_bpf__attach(obj)
	if cerr != 0 {
		return nil, fmt.Errorf("failed to attach BPF programs: %v", C.strerror(-cerr))
	}

	t := &Tracer{
		obj:       obj,
		stopChan:  make(chan struct{}),
		batchSize: defaultFlowMapOpsBatchSize,
	}

	if param.Stats {
		fd, err := enableBPFStats()
		if err != nil {
			return nil, err
		}
		t.statsFd = fd
	}

	return t, nil
}

// Close closes tracer.
func (t *Tracer) Close() {
	close(t.stopChan)
	if t.statsFd != 0 {
		syscall.Close(t.statsFd)
	}
	C.conntracer_bpf__destroy(t.obj)
}

// Start starts polling loop.
func (t *Tracer) Start(cb func([]*Flow) error, interval time.Duration) error {
	if err := initializeUDPPortBindingMap(t.udpPortBindingMapFD()); err != nil {
		return err
	}
	go t.pollFlows(cb, interval)
	return nil
}

// Stop stops polling loop.
func (t *Tracer) Stop() {
	t.stopChan <- struct{}{}
}

// DumpFlows gets and deletes all flows.
func (t *Tracer) DumpFlows() ([]*Flow, error) {
	eg := errgroup.Group{}
	flowChan := make(chan map[AggrFlowTuple]*Flow, 1)
	statChan := make(chan map[AggrFlowTuple]*AggrFlowStat, 1)
	eg.Go(func() error {
		flow, err := dumpAggrFlows(t.flowsMapFD())
		if err != nil {
			return err
		}
		flowChan <- flow
		close(flowChan)
		return nil
	})
	eg.Go(func() error {
		stats, err := dumpAggrFlowStats(t.flowStatsMapFD())
		if err != nil {
			return err
		}
		statChan <- stats
		close(statChan)
		return nil
	})
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// merge two maps
	flows := <-flowChan
	stats := <-statChan
	merged := make([]*Flow, 0, len(flows))
	for t, flow := range flows {
		if v, ok := stats[t]; ok {
			flow.Stat = v
		} else {
			flow.Stat = &AggrFlowStat{}
		}
		merged = append(merged, flow)
	}
	return merged, nil
}

func (t *Tracer) flowsMapFD() C.int {
	return C.bpf_map__fd(t.obj.maps.flows)
}

func (t *Tracer) flowStatsMapFD() C.int {
	return C.bpf_map__fd(t.obj.maps.flow_stats)
}

func (t *Tracer) udpPortBindingMapFD() C.int {
	return C.bpf_map__fd(t.obj.maps.udp_port_binding)
}

func (t *Tracer) pollFlows(cb func([]*Flow) error, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-t.stopChan:
			return
		case <-tick.C:
			flows, err := t.DumpFlows()
			if err != nil {
				log.Println(err)
			}
			if err := cb(flows); err != nil {
				log.Println(err)
			}
		}
	}
}

func dumpAggrFlows(fd C.int) (map[AggrFlowTuple]*Flow, error) {
	keys := make([]C.struct_aggregated_flow_tuple, C.MAX_ENTRIES)
	values := make([]C.struct_aggregated_flow, C.MAX_ENTRIES)

	nRead, err := dumpBpfMap(fd,
		unsafe.Pointer(&keys[0]), C.sizeof_struct_aggregated_flow_tuple,
		unsafe.Pointer(&values[0]), C.sizeof_struct_aggregated_flow,
		defaultFlowMapOpsBatchSize)
	if err != nil {
		return nil, err
	}

	flows := make(map[AggrFlowTuple]*Flow, nRead)
	for i := uint32(0); i < nRead; i++ {
		tuple := (AggrFlowTuple)(keys[i])
		saddr := inetNtop((uint32)(values[i].saddr))
		daddr := inetNtop((uint32)(values[i].daddr))
		flows[tuple] = &Flow{
			SAddr:       &saddr,
			DAddr:       &daddr,
			ProcessName: C.GoString((*C.char)(unsafe.Pointer(&values[i].task))),
			LPort:       (uint16)(values[i].lport),
			Direction:   flowDirectionFrom((C.flow_direction)(values[i].direction)),
			L4Proto:     (uint8)(ntohs((uint16)(values[i].l4_proto))),
			LastPID:     (uint32)(values[i].pid),
		}
	}

	return flows, nil
}

func dumpAggrFlowStats(fd C.int) (map[AggrFlowTuple]*AggrFlowStat, error) {
	keys := make([]C.struct_aggregated_flow_tuple, C.MAX_SINGLE_FLOW_ENTRIES)
	values := make([]C.struct_aggregated_flow_stat, C.MAX_SINGLE_FLOW_ENTRIES)

	nRead, err := dumpBpfMap(fd,
		unsafe.Pointer(&keys[0]), C.sizeof_struct_aggregated_flow_tuple,
		unsafe.Pointer(&values[0]), C.sizeof_struct_aggregated_flow_stat,
		defaultFlowMapOpsBatchSize)
	if err != nil {
		return nil, err
	}

	stats := make(map[AggrFlowTuple]*AggrFlowStat, nRead)
	for i := uint32(0); i < nRead; i++ {
		tuple := (AggrFlowTuple)(keys[i])
		stat := values[i]
		stats[tuple] = &AggrFlowStat{
			Timestamp: time.Unix((int64)(stat.ts_us)*1000*1000, 0),
			sentBytes: (uint64)(stat.sent_bytes),
			recvBytes: (uint64)(stat.recv_bytes),
		}
	}

	return stats, nil
}

// GetStats fetches stats of BPF program.
func (t *Tracer) GetStats() (map[int]*BpfProgramStats, error) {
	return getBPFAllStats(t.obj.obj)
}
