package collector

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// fieldValue builds an NVML field value whose union carries raw, in the host
// byte order NVML itself would have written.
func fieldValue(valueType nvml.ValueType, raw []byte) nvml.FieldValue {
	v := nvml.FieldValue{
		FieldId:    nvml.FI_DEV_MEMORY_TEMP,
		ValueType:  uint32(valueType),
		NvmlReturn: uint32(nvml.SUCCESS),
	}
	copy(v.Value[:], raw)
	return v
}

func u32Bytes(n uint32) []byte {
	b := make([]byte, 8)
	binary.NativeEndian.PutUint32(b, n)
	return b
}

func u64Bytes(n uint64) []byte {
	b := make([]byte, 8)
	binary.NativeEndian.PutUint64(b, n)
	return b
}

// i32Bytes and i64Bytes take signed arguments so a negative test value can be
// reinterpreted as unsigned bits; a direct conversion of a negative constant
// would not compile.
func i32Bytes(n int32) []byte { return u32Bytes(uint32(n)) }

func i64Bytes(n int64) []byte { return u64Bytes(uint64(n)) }

// TestNvmlFieldValueNumber_DecodesEachUnionType covers every value type NVML
// can tag a field with. Memory temperature arrives as an unsigned int, but the
// decoder is shared, and reading the union as the wrong width silently yields a
// plausible-looking wrong number rather than an error.
func TestNvmlFieldValueNumber_DecodesEachUnionType(t *testing.T) {
	tests := []struct {
		name      string
		valueType nvml.ValueType
		raw       []byte
		want      float64
	}{
		{"unsigned int", nvml.VALUE_TYPE_UNSIGNED_INT, u32Bytes(84), 84},
		{"unsigned short", nvml.VALUE_TYPE_UNSIGNED_SHORT, u32Bytes(72), 72},
		{"unsigned long", nvml.VALUE_TYPE_UNSIGNED_LONG, u64Bytes(96), 96},
		{"unsigned long long", nvml.VALUE_TYPE_UNSIGNED_LONG_LONG, u64Bytes(1 << 33), 1 << 33},
		{"signed int", nvml.VALUE_TYPE_SIGNED_INT, i32Bytes(-40), -40},
		{"signed long long", nvml.VALUE_TYPE_SIGNED_LONG_LONG, i64Bytes(-40), -40},
		{"double", nvml.VALUE_TYPE_DOUBLE, u64Bytes(math.Float64bits(61.5)), 61.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nvmlFieldValueNumber(fieldValue(tt.valueType, tt.raw))
			if !ok {
				t.Fatalf("nvmlFieldValueNumber(%s) returned ok=false", tt.name)
			}
			if got != tt.want {
				t.Errorf("nvmlFieldValueNumber(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestNvmlFieldValueNumber_UnknownTypeIsRejected ensures an unrecognised tag
// yields no value instead of a number decoded at a guessed width.
func TestNvmlFieldValueNumber_UnknownTypeIsRejected(t *testing.T) {
	for _, valueType := range []nvml.ValueType{nvml.VALUE_TYPE_COUNT, nvml.ValueType(99)} {
		got, ok := nvmlFieldValueNumber(fieldValue(valueType, u32Bytes(84)))
		if ok {
			t.Errorf("value type %d: ok = true, want false (got %v)", valueType, got)
		}
		if got != 0 {
			t.Errorf("value type %d: got %v, want 0", valueType, got)
		}
	}
}

// TestNvmlFieldNumber_AcceptsFullySuccessfulRead is the success path: the batch
// call and the field itself both report SUCCESS.
func TestNvmlFieldNumber_AcceptsFullySuccessfulRead(t *testing.T) {
	values := []nvml.FieldValue{fieldValue(nvml.VALUE_TYPE_UNSIGNED_INT, u32Bytes(84))}

	got, ok := nvmlFieldNumber(nvml.SUCCESS, values)
	if !ok {
		t.Fatal("nvmlFieldNumber returned ok=false for a fully successful read")
	}
	if got != 84 {
		t.Errorf("nvmlFieldNumber = %v, want 84", got)
	}
}

// TestNvmlFieldNumber_RejectsUnsuccessfulReads guards the bug this function
// exists to prevent. GetFieldValues reports success for the batch even when the
// single field in it is unsupported, so a per-field failure that is not checked
// turns uninitialised union bytes into a temperature reading.
func TestNvmlFieldNumber_RejectsUnsuccessfulReads(t *testing.T) {
	unsupported := fieldValue(nvml.VALUE_TYPE_UNSIGNED_INT, u32Bytes(84))
	unsupported.NvmlReturn = uint32(nvml.ERROR_NOT_SUPPORTED)

	tests := []struct {
		name   string
		ret    nvml.Return
		values []nvml.FieldValue
	}{
		{
			name:   "batch call failed",
			ret:    nvml.ERROR_NOT_SUPPORTED,
			values: []nvml.FieldValue{fieldValue(nvml.VALUE_TYPE_UNSIGNED_INT, u32Bytes(84))},
		},
		{
			name:   "batch succeeded but the field is unsupported",
			ret:    nvml.SUCCESS,
			values: []nvml.FieldValue{unsupported},
		},
		{
			name:   "no values returned",
			ret:    nvml.SUCCESS,
			values: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nvmlFieldNumber(tt.ret, tt.values)
			if ok {
				t.Errorf("nvmlFieldNumber returned ok=true, want false (got %v)", got)
			}
			if got != 0 {
				t.Errorf("nvmlFieldNumber = %v, want 0", got)
			}
		})
	}
}

// TestNvmlMemoryTemperature_RequestsTheMemoryTempField pins the field the
// collector asks for. Memory temperature is not a GetTemperature sensor:
// nvmlTemperatureSensors_t defines only TEMPERATURE_GPU, and its other member
// TEMPERATURE_COUNT is the enum bound. Requesting the wrong field id here would
// restore the original bug, in which memory temperature never reported at all.
func TestNvmlMemoryTemperature_RequestsTheMemoryTempField(t *testing.T) {
	if nvml.FI_DEV_MEMORY_TEMP != 82 {
		t.Errorf("FI_DEV_MEMORY_TEMP = %d, want 82", nvml.FI_DEV_MEMORY_TEMP)
	}
	if nvml.TEMPERATURE_COUNT != 1 {
		t.Errorf("TEMPERATURE_COUNT = %d, want 1; it is the enum bound, not a memory sensor",
			nvml.TEMPERATURE_COUNT)
	}
	if nvml.TEMPERATURE_GPU != 0 {
		t.Errorf("TEMPERATURE_GPU = %d, want 0", nvml.TEMPERATURE_GPU)
	}
}
