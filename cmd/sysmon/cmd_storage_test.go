//go:build !desktop

package main

import (
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestFilterDiskByIndex_Found(t *testing.T) {
	disks := []types.DiskInfo{
		{Index: 0, Name: "sda"},
		{Index: 1, Name: "nvme0n1"},
		{Index: 2, Name: "sdb"},
	}

	got, err := filterDiskByIndex(disks, 1)
	if err != nil {
		t.Fatalf("filterDiskByIndex() error = %v, want nil", err)
	}
	if got.Index != 1 {
		t.Errorf("got.Index = %d, want 1", got.Index)
	}
	if got.Name != "nvme0n1" {
		t.Errorf("got.Name = %q, want %q", got.Name, "nvme0n1")
	}
}

func TestFilterDiskByIndex_NotFound(t *testing.T) {
	disks := []types.DiskInfo{
		{Index: 0, Name: "sda"},
		{Index: 1, Name: "nvme0n1"},
		{Index: 2, Name: "sdb"},
	}

	_, err := filterDiskByIndex(disks, 5)
	if err == nil {
		t.Fatal("filterDiskByIndex() error = nil, want DiskIndexNotFoundError")
	}

	var target *types.DiskIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.DiskIndexNotFoundError", err)
	}
	if target.Index != 5 {
		t.Errorf("DiskIndexNotFoundError.Index = %d, want 5", target.Index)
	}
	if target.Available != 3 {
		t.Errorf("DiskIndexNotFoundError.Available = %d, want 3", target.Available)
	}
}

func TestFilterDiskByIndex_EmptySlice(t *testing.T) {
	_, err := filterDiskByIndex([]types.DiskInfo{}, 0)
	if err == nil {
		t.Fatal("filterDiskByIndex() error = nil, want DiskIndexNotFoundError")
	}

	var target *types.DiskIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.DiskIndexNotFoundError", err)
	}
	if target.Available != 0 {
		t.Errorf("DiskIndexNotFoundError.Available = %d, want 0", target.Available)
	}
}
