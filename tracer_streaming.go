package conntracer

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lelf -lz

#include <sys/resource.h>
#include <arpa/inet.h>
#include <errno.h>

#include <bpf/libbpf.h>
#include <bpf/bpf.h>
#include "conntracer_streaming.skel.h"
#include "conntracer.h"

extern int handleFlow(void *ctx, void *data, size_t size);

int libbpf_print_fn(enum libbpf_print_level level,
						const char *format, va_list args)
{
	// Ignore debug-level libbpf logs
	if (level > LIBBPF_INFO) {
		return 0;
	}
	return vfprintf(stderr, format, args);
}

void set_print_fn() {
	libbpf_set_print(libbpf_print_fn);
}

struct ring_buffer * new_ring_buf(int map_fd) {
	struct ring_buffer *rb = NULL;
	rb = ring_buffer__new(map_fd, handleFlow, NULL, NULL);
	if (rb < 0) {
		fprintf(stderr, "failed to cretae ring buffer!\n");
        return NULL;
	}
	return rb;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

const (
	// BPFRingbufPollingInterval is an interval of polling events in the ringbuffer.
	BPFRingbufPollingInterval = 50 * time.Millisecond
)

// TracerStreaming is an object for state retention without aggregation.
type TracerStreaming struct {
	obj      *C.struct_conntracer_streaming_bpf
	rb       *C.struct_ring_buffer
	stopChan chan struct{}
	statsFd  int
}

// NewTracerStreaming loads tracer without aggregation
func NewTracerStreaming(param *TracerParam) (*TracerStreaming, error) {
	C.set_print_fn()

	// Bump RLIMIT_MEMLOCK to allow BPF sub-system to do anything
	if err := bumpMemlockRlimit(); err != nil {
		return nil, err
	}

	obj := C.conntracer_streaming_bpf__open_and_load()
	if obj == nil {
		return nil, errors.New("failed to open and load BPF object")
	}

	ret, err := C.conntracer_streaming_bpf__attach(obj)
	if ret != 0 {
		C.conntracer_streaming_bpf__destroy(obj)
		return nil, fmt.Errorf("failed to attach BPF programs: %v", err)
	}

	// Set up BPF ring buffer polling.
	rb := C.new_ring_buf(C.bpf_map__fd(obj.maps.flows))
	if rb == nil {
		return nil, fmt.Errorf("failed to create ring buffer")
	}

	t := &TracerStreaming{
		obj:      obj,
		rb:       rb,
		stopChan: make(chan struct{}),
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

// TODO: sync.Pool
var globalFlowChan chan *Flow

// Start starts loop of polling events from kernel.
func (t *TracerStreaming) Start(fc chan *Flow) error {
	globalFlowChan = fc

	if err := initializeUDPPortBindingMap(t.udpPortBindingMapFD()); err != nil {
		return err
	}

	tick := time.NewTicker(BPFRingbufPollingInterval)
	defer tick.Stop()

	for {
		select {
		case <-t.stopChan:
			return nil
		case <-tick.C:
			n := C.ring_buffer__poll(t.rb, 10 /* timeout, ms */)
			if n < 0 {
				/* Ctrl-C will cause -EINTR */
				if syscall.Errno(-n) == syscall.EINTR {
					break
				}
				return fmt.Errorf("error polling ring buffer: %d", n)
			}
		}
	}
	return nil
}

// Stop stop loop of polling events.
func (t *TracerStreaming) Stop() {
	t.stopChan <- struct{}{}
}

// Close closes tracer.
func (t *TracerStreaming) Close() {
	close(t.stopChan)
	if t.statsFd != 0 {
		syscall.Close(t.statsFd)
	}
	C.ring_buffer__free(t.rb)
	C.conntracer_streaming_bpf__destroy(t.obj)
}

func (t *TracerStreaming) udpPortBindingMapFD() C.int {
	return C.bpf_map__fd(t.obj.maps.udp_port_binding)
}

// GetStats fetches stats of BPF program.
func (t *TracerStreaming) GetStats() (map[int]*BpfProgramStats, error) {
	return getBPFAllStats(t.obj.obj)
}
