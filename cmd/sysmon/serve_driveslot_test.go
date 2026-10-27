package main

import (
	"errors"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
)

func TestDriveSlotConfig_MapsTheBMCSection(t *testing.T) {
	got := driveSlotConfig(config.BMC{
		RedfishURL:         "https://10.0.0.5",
		Username:           "monitor",
		PasswordFile:       "/etc/go-sysmon/bmc-password",
		CAFile:             "/etc/go-sysmon/bmc-ca.pem",
		InsecureSkipVerify: true,
		TimeoutMs:          2500,
	})
	want := collector.DriveSlotConfig{
		RedfishURL:   "https://10.0.0.5",
		Username:     "monitor",
		PasswordFile: "/etc/go-sysmon/bmc-password",
		TLS: outbound.TLS{
			CAFile:             "/etc/go-sysmon/bmc-ca.pem",
			InsecureSkipVerify: true,
		},
		Timeout:    2500 * time.Millisecond,
		IPMIDevice: collector.DefaultIPMIDevice,
	}
	if got != want {
		t.Errorf("driveSlotConfig() = %+v, want %+v", got, want)
	}
}

func TestDriveSlotConfig_DefaultsConfigureTheCollector(t *testing.T) {
	sc := collector.NewSystemCollector(nil)
	if err := sc.ConfigureDriveSlots(driveSlotConfig(config.Defaults().BMC)); err != nil {
		t.Errorf("ConfigureDriveSlots(defaults) = %v, want nil: an unset bmc section is not an error", err)
	}
}

func TestDriveSlotConfig_UnusableSectionIsRefused(t *testing.T) {
	sc := collector.NewSystemCollector(nil)
	err := sc.ConfigureDriveSlots(driveSlotConfig(config.BMC{RedfishURL: "https://10.0.0.5"}))
	var cfgErr *collector.RedfishConfigError
	if !errors.As(err, &cfgErr) {
		t.Errorf("ConfigureDriveSlots(no credentials) = %v, want *RedfishConfigError", err)
	}
}
