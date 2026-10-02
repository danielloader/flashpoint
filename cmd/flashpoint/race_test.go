//go:build race

package main

func init() { buildFlags = append(buildFlags, "-race") }
