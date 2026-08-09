# Makefile for the llmux C++ examples.
#
#   make            build both
#   make run        build both and run them against a fake upstream
#   make clean
#
# llmux.hpp is header-only; there is nothing to build but the examples.
# direct_chat links libllmux, sidecar_chat deliberately does not (it forks).

CXX      ?= c++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra -Wpedantic
ROOT     := $(abspath ../..)

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),Darwin)
  GOOS := darwin
  LIBNAME := libllmux.dylib
else
  GOOS := linux
  LIBNAME := libllmux.so
endif
ifeq ($(UNAME_M),arm64)
  GOARCH := arm64
else ifeq ($(UNAME_M),aarch64)
  GOARCH := arm64
else
  GOARCH := amd64
endif

LLMUX_LIB_DIR ?= $(ROOT)/dist/ffi/$(GOOS)_$(GOARCH)
INCLUDE       := -I$(ROOT)/ffi/include -I.

BINS := direct_chat sidecar_chat

.PHONY: all run clean check-lib

all: $(BINS)

check-lib:
	@test -f "$(LLMUX_LIB_DIR)/$(LIBNAME)" || { \
	  echo "no $(LIBNAME) in $(LLMUX_LIB_DIR)"; \
	  echo "build it:  (cd $(ROOT) && scripts/build-ffi.sh)"; \
	  echo "or point at one:  make LLMUX_LIB_DIR=/path/to/dir"; \
	  exit 1; }

direct_chat: direct_chat.cpp llmux.hpp | check-lib
	$(CXX) $(CXXFLAGS) $(INCLUDE) -o $@ direct_chat.cpp \
	  -L$(LLMUX_LIB_DIR) -lllmux -Wl,-rpath,$(LLMUX_LIB_DIR) -lpthread
ifeq ($(UNAME_S),Darwin)
	@# cgo gives the dylib the bare install name "libllmux.dylib" with no
	@# @rpath prefix, so the -rpath above is never consulted. See ../c/Makefile.
	install_name_tool -change libllmux.dylib @rpath/libllmux.dylib $@
endif

sidecar_chat: sidecar_chat.cpp
	$(CXX) $(CXXFLAGS) -o $@ sidecar_chat.cpp

run: $(BINS)
	./run-demo.sh

clean:
	rm -f $(BINS)
	rm -rf *.dSYM
