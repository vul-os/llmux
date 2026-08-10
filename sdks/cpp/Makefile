# Makefile for the llmux C++ examples.
#
#   make            build both
#   make run        build both and run them against a fake upstream
#   make clean
#
# llmux.hpp is header-only; there is nothing to build but the examples.
# direct_chat links libllmux, sidecar_chat deliberately does not (it forks).
#
# RAISED TO C++20, deliberately: the whole point of this pair of examples is to
# demonstrate the std::jthread/std::stop_token cancellation overload, which
# does not exist below C++20. llmux.hpp itself still compiles fine under
# C++17 — the overload is feature-tested on __cpp_lib_jthread and simply is not
# declared without it — so a consumer building their own program at -std=c++17
# loses nothing but that one overload.
CXX      ?= c++
CXXFLAGS ?= -std=c++20 -O2 -Wall -Wextra -Wpedantic
ROOT     := $(abspath ../..)

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),Darwin)
  GOOS := darwin
  LIBNAME := libllmux.dylib
  # Apple's libc++ (Xcode/CLT through at least clang 17) ships <stop_token> but
  # guards the actual class definitions behind _LIBCPP_HAS_NO_EXPERIMENTAL_STOP_TOKEN,
  # defined unless this flag is passed — without it __cpp_lib_jthread is never
  # defined and our overload silently does not exist, rather than failing to
  # build. Measured on this machine: Apple clang 17.0.0, macOS 15.7, CLT SDK.
  # GCC's libstdc++ and recent upstream LLVM libc++ need no such flag; this is
  # an Apple-specific wart, not a C++20 one, so it is scoped to Darwin only —
  # passing it to a compiler that does not recognise it is a hard error.
  CXXFLAGS += -fexperimental-library
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
