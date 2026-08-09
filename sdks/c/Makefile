# Makefile for the llmux C examples.
#
#   make            build both
#   make run        build both and run them against a fake upstream
#   make clean
#
# direct_chat LINKS against libllmux, which is the shape a program with an
# installed library has. (ffi/ctest/smoke.c dlopen()s it instead, because a test
# that resolves every symbol by name is what catches a missing //export.)
#
# The library lives in dist/ffi/<goos>_<goarch>/ after scripts/build-ffi.sh; set
# LLMUX_LIB_DIR to override. -rpath is baked in so the built example runs from
# a checkout without DYLD_LIBRARY_PATH.

CC      ?= cc
CFLAGS  ?= -std=c11 -O2 -Wall -Wextra -Wpedantic
ROOT    := $(abspath ../..)

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

direct_chat: direct_chat.c jsonpeek.c jsonpeek.h | check-lib
	$(CC) $(CFLAGS) $(INCLUDE) -o $@ direct_chat.c jsonpeek.c \
	  -L$(LLMUX_LIB_DIR) -lllmux -Wl,-rpath,$(LLMUX_LIB_DIR) -lpthread
ifeq ($(UNAME_S),Darwin)
	@# `go build -buildmode=c-shared` gives the dylib the bare install name
	@# "libllmux.dylib" — no @rpath prefix — so the -rpath above is never
	@# consulted and the loader fails with "Library not loaded: libllmux.dylib".
	@# This rewrites the reference in OUR executable, which is the fix that does
	@# not require modifying the library. Packagers should instead ship the
	@# dylib with a proper install name:
	@#   install_name_tool -id @rpath/libllmux.dylib libllmux.dylib
	install_name_tool -change libllmux.dylib @rpath/libllmux.dylib $@
endif

# No libllmux here on purpose: the sidecar example forks, and a process that
# has loaded the Go runtime must not.
sidecar_chat: sidecar_chat.c mini_http.c mini_http.h jsonpeek.c jsonpeek.h
	$(CC) $(CFLAGS) $(INCLUDE) -o $@ sidecar_chat.c mini_http.c jsonpeek.c

run: $(BINS)
	./run-demo.sh

clean:
	rm -f $(BINS)
