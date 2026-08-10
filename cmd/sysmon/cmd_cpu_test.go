//go:build !desktop

package main

import (
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestFilterCPUByIndex_Found(t *testing.T) {
	cpus := []types.CPUInfo{
		{Index: 0, ModelName: "CPU-0"},
		{Index: 1, ModelName: "CPU-1"},
		{Index: 2, ModelName: "CPU-2"},
	}

	got, err := filterCPUByIndex(cpus, 1)
	if err != nil {
		t.Fatalf("filterCPUByIndex() error = %v, want nil", err)
	}
	if got.Index != 1 {
		t.Errorf("got.Index = %d, want 1", got.Index)
	}
	if got.ModelName != "CPU-1" {
		t.Errorf("got.ModelName = %q, want %q", got.ModelName, "CPU-1")
	}
}

func TestFilterCPUByIndex_NotFound(t *testing.T) {
	cpus := []types.CPUInfo{
		{Index: 0, ModelName: "CPU-0"},
		{Index: 1, ModelName: "CPU-1"},
		{Index: 2, ModelName: "CPU-2"},
	}

	_, err := filterCPUByIndex(cpus, 5)
	if err == nil {
		t.Fatal("filterCPUByIndex() error = nil, want CPUIndexNotFoundError")
	}

	var target *types.CPUIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.CPUIndexNotFoundError", err)
	}
	if target.Index != 5 {
		t.Errorf("CPUIndexNotFoundError.Index = %d, want 5", target.Index)
	}
	if target.Available != 3 {
		t.Errorf("CPUIndexNotFoundError.Available = %d, want 3", target.Available)
	}
}

func TestFilterCPUByIndex_EmptySlice(t *testing.T) {
	_, err := filterCPUByIndex([]types.CPUInfo{}, 0)
	if err == nil {
		t.Fatal("filterCPUByIndex() error = nil, want CPUIndexNotFoundError")
	}

	var target *types.CPUIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.CPUIndexNotFoundError", err)
	}
	if target.Available != 0 {
		t.Errorf("CPUIndexNotFoundError.Available = %d, want 0", target.Available)
	}
}
