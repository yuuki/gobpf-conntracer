// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
// Copyright (c) 2020 Yuuki Tsubouchi

#include "vmlinux.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>

#include "conntracer.h"
#include "port_binding.h"

#define AF_INET		2
#define AF_INET6	10

char LICENSE[] SEC("license") = "Dual BSD/GPL";

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, u32);
	__type(value, struct sock *);
	__uint(map_flags, BPF_F_NO_PREALLOC);
} tcp_connect_sockets SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024 /* 256 KB */);
} flows SEC(".maps");

static __always_inline void
insert_flows(pid_t pid, struct sock *sk, __u16 lport, __u8 direction)
{
	struct flow *flow;

	flow = bpf_ringbuf_reserve(&flows, sizeof(*flow), 0);
	if (!flow) {
		log_debug("insert_tcp_flows: could not reserve ringbuf pid:%d\n", pid);
		return;
	}

	flow->ts_us = bpf_ktime_get_ns() / 1000;
	BPF_CORE_READ_INTO(&flow->saddr, sk, __sk_common.skc_rcv_saddr);
	BPF_CORE_READ_INTO(&flow->daddr, sk, __sk_common.skc_daddr);
	flow->lport = lport;
	flow->pid = pid;
	flow->direction = direction;
	bpf_get_current_comm(&flow->task, sizeof(flow->task));

    bpf_ringbuf_submit(flow, 0);
}

static __always_inline void
insert_udp_flows(pid_t pid, struct ipv4_flow_key* flow_key)
{
	struct flow *flow;

	flow = bpf_ringbuf_reserve(&flows, sizeof(*flow), 0);
	if (!flow) {
		log_debug("insert_udp_flows: could not reserve ringbuf pid:%d\n", pid);
		return;
	}

	flow->ts_us = bpf_ktime_get_ns() / 1000;
	flow->saddr = flow_key->saddr;
	flow->daddr = flow_key->daddr;
	flow->lport = flow_key->lport;
	flow->direction = flow_key->direction;
	flow->l4_proto = flow_key->l4_proto;
	flow->pid = pid;
	bpf_get_current_comm(flow->task, sizeof(flow->task));

    bpf_ringbuf_submit(flow, 0);
}

SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(tcp_v4_connect, struct sock *sk)
{
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	__u32 pid = pid_tgid >> 32;
	__u32 tid = pid_tgid;

	bpf_map_update_elem(&tcp_connect_sockets, &tid, &sk, BPF_ANY);

	log_debug("kprobe/tcp_v4_connect: pid_tgid:%d\n", pid_tgid);
	return 0;
}

SEC("kretprobe/tcp_v4_connect")
int BPF_KRETPROBE(tcp_v4_connect_ret, int ret)
{
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	__u32 pid = pid_tgid >> 32;
	__u32 tid = pid_tgid;
	__u16 dport = 0;

	struct sock** skpp = bpf_map_lookup_elem(&tcp_connect_sockets, &tid);
	if (!skpp)
		return 0;

	if (ret)
		goto end;

	struct sock* sk = *skpp;

	BPF_CORE_READ_INTO(&dport, sk, __sk_common.skc_dport);

	insert_flows(pid, sk, dport, FLOW_ACTIVE);

end:
	bpf_map_delete_elem(&tcp_connect_sockets, &tid);
	log_debug("kretprobe/tcp_v4_connect: pid_tgid:%d, dport:%d\n", pid_tgid, dport);
	return 0;
}

SEC("kretprobe/inet_csk_accept")
int BPF_KRETPROBE(inet_csk_accept_ret, struct sock *sk)
{
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	__u32 pid = pid_tgid >> 32;
	__u16 lport = 0;

	if (!sk)
		return 0;

	BPF_CORE_READ_INTO(&lport, sk, __sk_common.skc_num);

	insert_flows(pid, sk, lport, FLOW_PASSIVE);

	log_debug("kretprobe/inet_csk_accept: pid_tgid:%d, lport:%d\n", pid_tgid, lport);
	return 0;
}

// struct sock with udp_sendmsg may not miss ip addresses on listening socket.
// Addresses are retrieved from struct flowi4 with ip_make_skb.
// https://github.com/DataDog/datadog-agent/pull/6307
SEC("kprobe/ip_make_skb")
int BPF_KPROBE(ip_make_skb, struct sock *sk, struct flowi4 *flw4) {
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	__u32 pid = pid_tgid >> 32;
	__u16 dport, sport;
	struct ipv4_flow_key flow_key = {};

	BPF_CORE_READ_INTO(&sport, sk, __sk_common.skc_num);
	BPF_CORE_READ_INTO(&dport, sk, __sk_common.skc_dport);

	__u8 *sstate = bpf_map_lookup_elem(&udp_port_binding, &sport);
	if (sstate) {
		BPF_CORE_READ_INTO(&flow_key.saddr, flw4, daddr);
		BPF_CORE_READ_INTO(&flow_key.daddr, flw4, saddr);
		flow_key.direction = FLOW_PASSIVE;
		flow_key.lport = bpf_htons(sport);
	} else {
		BPF_CORE_READ_INTO(&flow_key.saddr, flw4, saddr);
		BPF_CORE_READ_INTO(&flow_key.daddr, flw4, daddr);
		flow_key.direction = FLOW_ACTIVE;
		flow_key.lport = dport;
	}
	flow_key.l4_proto = IPPROTO_UDP;

	insert_udp_flows(pid, &flow_key);

	log_debug("kprobe/udp_sendmsg: lport:%u, tgid:%u\n", sport, pid_tgid);
	return 0;
}

// struct sock with udp_recvmsg may not miss ip addresses on listening socket.
// Addresses are retrieved from arguments of skb_consume_udp.
SEC("kprobe/skb_consume_udp")
int BPF_KPROBE(skb_consume_udp, struct sock *sk, struct sk_buff *skb) {
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	__u32 pid = pid_tgid >> 32;

	struct udphdr *udphdr = (struct udphdr *)(BPF_CORE_READ(skb, head)
		+ BPF_CORE_READ(skb,transport_header));
	struct iphdr *iphdr = (struct iphdr *)(BPF_CORE_READ(skb, head) 
		+ BPF_CORE_READ(skb, network_header));

	struct ipv4_flow_key flow_key = {};
	__u16 sport = BPF_CORE_READ(udphdr, source);
	__u16 dport = BPF_CORE_READ(udphdr, dest);

	__u16 dport_key = bpf_htons(dport);
	__u8 *sstate = bpf_map_lookup_elem(&udp_port_binding, &dport_key);
	if (sstate) {
		flow_key.saddr = BPF_CORE_READ(iphdr, saddr);
		flow_key.daddr = BPF_CORE_READ(iphdr, daddr);
		flow_key.direction = FLOW_PASSIVE;
		flow_key.lport = dport;
	} else {
		flow_key.saddr = BPF_CORE_READ(iphdr, daddr);
		flow_key.daddr = BPF_CORE_READ(iphdr, saddr);
		flow_key.direction = FLOW_ACTIVE;
		flow_key.lport = sport;
	}

	flow_key.l4_proto = IPPROTO_UDP;

	insert_udp_flows(pid, &flow_key);

    log_debug("kprobe/skb_consume_udp: sport:%u, dport:%u, tid:%u\n",
		sport, dport, pid_tgid);
    return 0;
}


// for tracking UDP listening state
SEC("tracepoint/syscalls/sys_enter_socket")
int tracepoint__syscalls__sys_enter_socket(struct trace_event_raw_sys_enter* ctx) {
	__u64 tid = bpf_get_current_pid_tgid();
	int family = (int)ctx->args[0];
	int type = (int)ctx->args[1];
	log_debug("tp/sys_enter_socket: family=%u, type=%u, tid=%u\n", family, type, tid);

	return sys_enter_socket(family, type, tid);
}

// for tracking UDP listening state
SEC("tracepoint/syscalls/sys_exit_socket")
int tracepoint__syscalls__sys_exit_socket(struct trace_event_raw_sys_exit* ctx) {
    __u64 tid = bpf_get_current_pid_tgid();
    log_debug("tp/sys_exit_socket: fd=%d, tid=%u\n", ctx->ret, tid);

	return sys_exit_socket(ctx->ret, tid);
}

SEC("tracepoint/syscalls/sys_enter_bind")
int tracepoint__syscalls__sys_enter_bind(struct trace_event_raw_sys_enter* ctx) {
    __u64 tid = bpf_get_current_pid_tgid();
	int fd = (int)ctx->args[0];
	const struct sockaddr *addr = (const struct sockaddr *)ctx->args[1];

    log_debug("tp/sys_enter_bind: fd=%u, addr=%x, tid=%u\n", fd, addr, tid);
	return sys_enter_bind(fd, addr, tid);
}

SEC("tracepoint/syscalls/sys_exit_bind")
int tracepoint__syscalls__sys_exit_bind(struct trace_event_raw_sys_exit* ctx) {
    __u64 tid = bpf_get_current_pid_tgid();

    log_debug("tp/sys_exit_bind: ret=%d, tid=%u\n", ctx->ret, tid);
	return sys_exit_bind(ctx->ret, tid);
}