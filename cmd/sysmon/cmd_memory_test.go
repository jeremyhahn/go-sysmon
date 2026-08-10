//go:build !desktop

package main

import (
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestFilterDIMMByIndex_Found(t *testing.T) {
	dimms := []types.DIMMInfo{
		{Index: 0, Location: "DIMM_A1"},
		{Index: 1, Location: "DIMM_A2"},
		{Index: 2, Location: "DIMM_B1"},
	}

	got, err := filterDIMMByIndex(dimms, 1)
	if err != nil {
		t.Fatalf("filterDIMMByIndex() error = %v, want nil", err)
	}
	if got.Index != 1 {
		t.Errorf("got.Index = %d, want 1", got.Index)
	}
	if got.Location != "DIMM_A2" {
		t.Errorf("got.Location = %q, want %q", got.Location, "DIMM_A2")
	}
}

func TestFilterDIMMByIndex_NotFound(t *testing.T) {
	dimms := []types.DIMMInfo{
		{Index: 0, Location: "DIMM_A1"},
		{Index: 1, Location: "DIMM_A2"},
		{Index: 2, Location: "DIMM_B1"},
	}

	_, err := filterDIMMByIndex(dimms, 5)
	if err == nil {
		t.Fatal("filterDIMMByIndex() error = nil, want DIMMIndexNotFoundError")
	}

	var target *types.DIMMIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.DIMMIndexNotFoundError", err)
	}
	if target.Index != 5 {
		t.Errorf("DIMMIndexNotFoundError.Index = %d, want 5", target.Index)
	}
	if target.Available != 3 {
		t.Errorf("DIMMIndexNotFoundError.Available = %d, want 3", target.Available)
	}
}

func TestFilterDIMMByIndex_EmptySlice(t *testing.T) {
	_, err := filterDIMMByIndex([]types.DIMMInfo{}, 0)
	if err == nil {
		t.Fatal("filterDIMMByIndex() error = nil, want DIMMIndexNotFoundError")
	}

	var target *types.DIMMIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.DIMMIndexNotFoundError", err)
	}
	if target.Available != 0 {
		t.Errorf("DIMMIndexNotFoundError.Available = %d, want 0", target.Available)
	}
}
