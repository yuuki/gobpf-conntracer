TOOL := conntop

GO := $(shell which go)
GO_SRC := $(shell find . -type f -name '*.go')
GOLINT = $$(go env GOPATH)/bin/golint -set_exit_status $$(go list -mod=vendor ./...)

SUDO := sudo -E
OUTPUT := .output
CMD_CLANG ?= clang
CMD_DOCKER ?= docker
LLVM_STRIP ?= llvm-strip
BPFTOOL ?= $(abspath tools/bpftool)
LIBBPF_SRC := $(abspath libbpf/src)
LIBBPF_OBJ := $(abspath $(OUTPUT)/libbpf.a)
BPF_SRC_DIR := bpf
INCLUDE_DIR := $(abspath include)
INCLUDES := -I$(OUTPUT) -I$(INCLUDE_DIR)
CFLAGS := -g -Wall
ARCH_UNAME := $(shell uname -m)
ARCH ?= $(ARCH_UNAME:aarch64=arm64)
CLANG_FLAGS := -g -O2 -target bpf -fPIE
BPF_DEBUG ?= 0
ifeq ($(BPF_DEBUG), 1)
    CLANG_FLAGS += -DDEBUG
else
    CLANG_FLAGS += -DNDEBUG
endif

DOCKER_BUILDER ?= $(TOOL)-builder
OUT_DOCKER ?= conntracer-conntop

BPF_PROGS = conntracer conntracer_streaming conntracer_in_flow_aggr

msg = @printf '  %-8s %s%s\n'                       \
                "$(1)"                                          \
                "$(patsubst $(abspath $(OUTPUT))/%,%,$(2))"     \
                "$(if $(3), $(3))";
MAKEFLAGS += --no-print-directory

.PHONY: all
all: bpf $(TOOL)

#--- libbpf ---

$(LIBBPF_SRC):
	test -d $(LIBBPF_SRC) || git submodule update --init || (echo "missing libbpf source" ; false)

$(OUTPUT) $(OUTPUT)/libbpf:
	$(call msg,MKDIR,$@)
	@mkdir -p $@

# Build libbpf
$(LIBBPF_OBJ): $(wildcard $(LIBBPF_SRC)/*.[ch] $(LIBBPF_SRC)/Makefile) | $(OUTPUT)/libbpf
	$(call msg,LIB,$@)
	$(MAKE) -C $(LIBBPF_SRC) BUILD_STATIC_ONLY=1                      \
			OBJDIR=$(dir $@)/libbpf DESTDIR=$(dir $@)                     \
			INCLUDEDIR= LIBDIR= UAPIDIR=                          \
			install
	@ranlib $@

#--- Kernel-space code --- 

# Build BPF code
linux_arch := $(ARCH:x86_64=x86)
$(OUTPUT)/%.bpf.o: $(BPF_SRC_DIR)/%.bpf.c $(LIBBPF_OBJ) $(wildcard %.h) $(BPF_SRC_DIR)/vmlinux.h | $(OUTPUT)
	$(call msg,BPF,$@)
	@$(CMD_CLANG) $(CLANG_FLAGS) -D__TARGET_ARCH_$(linux_arch) $(INCLUDES) -c $(filter %.c,$^) -o $@
	@$(LLVM_STRIP) -g $@ # strip useless DWARF info

# Generate BPF skeletons
$(INCLUDE_DIR)/%.skel.h: $(OUTPUT)/%.bpf.o | $(OUTPUT)
	$(call msg,GEN-SKEL,$@)
	@$(BPFTOOL) gen skeleton $< > $@

.PHONY: bpf
ifndef DOCKER
bpf: goclean $(patsubst %,$(INCLUDE_DIR)/%.skel.h,$(BPF_PROGS))
else
bpf: $(DOCKER_BUILDER)
	$(call docker_builder_make,$@)
endif

.PHONY: bpf/clean
bpf/clean:
	rm -f $(OUTPUT)/*.bpf.o $(INCLUDE_DIR)/*.skel.h

#--- User-space code ---

go_env := GOOS=linux GOARCH=$(ARCH:x86_64=amd64) CGO_CFLAGS="-I $(INCLUDE_DIR) -Wno-implicit-function-declaration" CGO_LDFLAGS="$(abspath $(LIBBPF_OBJ)) -lelf -lz"

ifndef DOCKER
$(TOOL): bpf $(LIBBPF_OBJ) $(filter-out *_test.go,$(GO_SRC))
	$(call msg,BINARY,$@)
	@$(go_env) $(GO) build -mod vendor ./tools/$@
else 
$(TOOL): $(DOCKER_BUILDER)
	$(call docker_builder_make,$@)
endif

.PHONY: verify
ifndef DOCKER
verify: bpf $(LIBBPF_OBJ)
	$(call msg,VERIFY)
	@$(go_env) $(GO) build -mod vendor ./tools/verifier
	@$(SUDO) ./verifier
else 
verify: $(DOCKER_BUILDER)
	$(call docker_builder_make,$@)
endif

.PHONY: test
ifndef DOCKER
test: bpf $(LIBBPF_OBJ)
	$(call msg,TEST)
	@$(go_env) $(SUDO) $(GO) test -v .
else
test: $(DOCKER_BUILDER)
	$(call docker_builder_make,$@)
endif

.PHONY: lint
lint: $(filter-out *_test.go,$(GO_SRC))
	$(call msg,LINT)
	@$(GOLINT)

.PHONY: tidy
tidy:
	$(call msg,TIDY)
	@go mod tidy
	@go mod vendor

.PNONY: goclean
goclean:
	@go clean -x -cache -testcache >/dev/null

docker_builder_file := $(OUTPUT)/$(DOCKER_BUILDER)
.PHONY: $(DOCKER_BUILDER)
$(DOCKER_BUILDER) $(docker_builder_file) &: tools/$(TOOL)/Dockerfile.builder | $(OUTPUT)
	$(CMD_DOCKER) build -t $(DOCKER_BUILDER) --iidfile $(docker_builder_file) - < $<

define docker_builder_make
	$(CMD_DOCKER) run --rm -v $(abspath .):/conntop \
	--entrypoint make $(DOCKER_BUILDER) $(1)
endef

.PHONY: clean
clean: goclean
	$(call msg,CLEAN)
	-rm -rf $(OUTPUT) $(TOOL)
	-$(CMD_DOCKER) rmi $(file < $(docker_builder_file))

.PHONY: docker
docker:
	$(CMD_DOCKER) build -t $(OUT_DOCKER):latest .

# delete failed targets
.DELETE_ON_ERROR:

# keep intermediate (.skel.h, .bpf.o, etc) targets
.SECONDARY:
