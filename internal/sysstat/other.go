//go:build !darwin && !linux

package sysstat

import "context"

func processTree(context.Context, int) []proc { return nil }
func diskFree(string) (uint64, bool)          { return 0, false }
