# go-conntracer-bpf

go-conntracer-bpf is a library for Go for tracing network connection (TCP) events (connect, accept, close) on BPF kprobe inspired by [weaveworks/tcptracer-bpf](https://github.com/weaveworks/tcptracer-bpf).

## Features

- Low-overhead tracing by aggregating connections events in kernel.
- BPF CO-RE (Compile Once – Run Everywhere)-enabled

## Requirements

### Compilation phase

- libbpf (included as git submodule)
- Clang/LLVM 10+
- libelf-dev and libz-dev packages.

### Execution phase

- Linux kernel to be built with BTF type information. See <https://github.com/libbpf/libbpf#bpf-co-re-compile-once--run-everywhere>.

## Projects using go-conntracer-bpf

- [yuuki/shawk](https://github.com/yuuki/shawk)
