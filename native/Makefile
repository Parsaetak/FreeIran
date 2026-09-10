# FreeIran native acceleration layer.
#
# Builds libfreeiran_native.a (static), libfreeiran_native.so/.dll
# (shared) and the native unit tests.
#
#   make            build library + tests
#   make test       build and run tests
#   make clean
#
# The library has no external dependencies and requires only a
# C++17-capable compiler.

CXX ?= g++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra -Wpedantic -fno-exceptions \
            -fvisibility=hidden -fPIC
LDFLAGS ?=

SRCDIR  := src
INCDIR  := include
TESTDIR := tests
BUILDDIR := build

STATIC_LIB := $(BUILDDIR)/libfreeiran_native.a
SHARED_LIB := $(BUILDDIR)/libfreeiran_native.so
TEST_BIN   := $(BUILDDIR)/test_native

HEADERS := $(INCDIR)/freeiran.h
SOURCES := $(SRCDIR)/freeiran.cpp

.PHONY: all test clean

all: $(STATIC_LIB) $(SHARED_LIB) $(TEST_BIN)

$(BUILDDIR):
	mkdir -p $(BUILDDIR)

$(BUILDDIR)/freeiran.o: $(SOURCES) $(HEADERS) | $(BUILDDIR)
	$(CXX) $(CXXFLAGS) -I$(INCDIR) -c $< -o $@

$(STATIC_LIB): $(BUILDDIR)/freeiran.o
	ar rcs $@ $^

$(SHARED_LIB): $(BUILDDIR)/freeiran.o
	$(CXX) -shared -fPIC -o $@ $^ $(LDFLAGS)

$(TEST_BIN): $(TESTDIR)/test_native.cpp $(BUILDDIR)/freeiran.o | $(BUILDDIR)
	$(CXX) $(CXXFLAGS) -I$(INCDIR) $< $(BUILDDIR)/freeiran.o -o $@ $(LDFLAGS)

test: $(TEST_BIN)
	./$(TEST_BIN)

clean:
	rm -rf $(BUILDDIR)
