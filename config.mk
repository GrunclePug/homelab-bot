# homelab-bot version
VERSION = 0.3.1

APP = homelab-bot

# paths
PREFIX ?= /usr/local

# tools
GO = go

# flags
# -s: disable symbol table
# -w: disable DWARF generation
# -X: inject version string
LDFLAGS = -s -w -X main.Version=$(VERSION)
