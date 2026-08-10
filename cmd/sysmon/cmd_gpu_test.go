//go:build !desktop

package main

import (
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestFilterGPUByIndex_Found(t *testing.T) {
	gpus := []types.GPUInfo{
		{Index: 0, Name: "GPU-0"},
		{Index: 1, Name: "GPU-1"},
		{Index: 2, Name: "GPU-2"},
	}

	got, err := filterGPUByIndex(gpus, 1)
	if err != nil {
		t.Fatalf("filterGPUByIndex() error = %v, want nil", err)
	}
	if got.Index != 1 {
		t.Errorf("got.Index = %d, want 1", got.Index)
	}
	if got.Name != "GPU-1" {
		t.Errorf("got.Name = %q, want %q", got.Name, "GPU-1")
	}
}

func TestFilterGPUByIndex_NotFound(t *testing.T) {
	gpus := []types.GPUInfo{
		{Index: 0, Name: "GPU-0"},
		{Index: 1, Name: "GPU-1"},
		{Index: 2, Name: "GPU-2"},
	}

	_, err := filterGPUByIndex(gpus, 5)
	if err == nil {
		t.Fatal("filterGPUByIndex() error = nil, want GPUIndexNotFoundError")
	}

	var target *types.GPUIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.GPUIndexNotFoundError", err)
	}
	if target.Index != 5 {
		t.Errorf("GPUIndexNotFoundError.Index = %d, want 5", target.Index)
	}
	if target.Available != 3 {
		t.Errorf("GPUIndexNotFoundError.Available = %d, want 3", target.Available)
	}
}

func TestFilterGPUByIndex_EmptySlice(t *testing.T) {
	_, err := filterGPUByIndex([]types.GPUInfo{}, 0)
	if err == nil {
		t.Fatal("filterGPUByIndex() error = nil, want GPUIndexNotFoundError")
	}

	var target *types.GPUIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.GPUIndexNotFoundError", err)
	}
	if target.Available != 0 {
		t.Errorf("GPUIndexNotFoundError.Available = %d, want 0", target.Available)
	}
}
