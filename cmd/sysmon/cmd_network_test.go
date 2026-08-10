//go:build !desktop

package main

import (
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestFilterNetworkByIndex_Found(t *testing.T) {
	nets := []types.NetworkInfo{
		{Index: 0, Name: "eth0"},
		{Index: 1, Name: "wlan0"},
		{Index: 2, Name: "lo"},
	}

	got, err := filterNetworkByIndex(nets, 1)
	if err != nil {
		t.Fatalf("filterNetworkByIndex() error = %v, want nil", err)
	}
	if got.Index != 1 {
		t.Errorf("got.Index = %d, want 1", got.Index)
	}
	if got.Name != "wlan0" {
		t.Errorf("got.Name = %q, want %q", got.Name, "wlan0")
	}
}

func TestFilterNetworkByIndex_NotFound(t *testing.T) {
	nets := []types.NetworkInfo{
		{Index: 0, Name: "eth0"},
		{Index: 1, Name: "wlan0"},
		{Index: 2, Name: "lo"},
	}

	_, err := filterNetworkByIndex(nets, 5)
	if err == nil {
		t.Fatal("filterNetworkByIndex() error = nil, want NetworkIndexNotFoundError")
	}

	var target *types.NetworkIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.NetworkIndexNotFoundError", err)
	}
	if target.Index != 5 {
		t.Errorf("NetworkIndexNotFoundError.Index = %d, want 5", target.Index)
	}
	if target.Available != 3 {
		t.Errorf("NetworkIndexNotFoundError.Available = %d, want 3", target.Available)
	}
}

func TestFilterNetworkByIndex_EmptySlice(t *testing.T) {
	_, err := filterNetworkByIndex([]types.NetworkInfo{}, 0)
	if err == nil {
		t.Fatal("filterNetworkByIndex() error = nil, want NetworkIndexNotFoundError")
	}

	var target *types.NetworkIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *types.NetworkIndexNotFoundError", err)
	}
	if target.Available != 0 {
		t.Errorf("NetworkIndexNotFoundError.Available = %d, want 0", target.Available)
	}
}
