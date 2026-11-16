package capsulad

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// The native engine speaks canonical protobuf JSON: lowerCamelCase field
// names, enum names, base64 bytes and 64-bit integers encoded as JSON
// strings. encoding/json alone neither produces nor accepts that shape, so
// these two helpers implement the conversion for both directions while
// remaining compatible with ordinary Go structs.

var (
	jsonMarshalerType   = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	jsonRawMessageType  = reflect.TypeOf(json.RawMessage(nil))
)

// marshalProtoJSON encodes a Go value as canonical protobuf JSON.
func marshalProtoJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeProtoValue(&buf, reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeProtoValue(buf *bytes.Buffer, value reflect.Value) error {
	if !value.IsValid() {
		buf.WriteString("null")
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return encodeProtoValue(buf, value.Elem())
	}
	if value.Type() == jsonRawMessageType {
		raw, _ := value.Interface().(json.RawMessage)
		buf.Write(raw)
		return nil
	}
	if value.Type().Implements(jsonMarshalerType) {
		if isNilable(value.Kind()) && value.IsNil() {
			buf.WriteString("null")
			return nil
		}
		encoded, err := json.Marshal(value.Interface())
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	}

	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return encodeProtoValue(buf, value.Elem())
	case reflect.Bool:
		if value.Bool() {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case reflect.String:
		encoded, err := json.Marshal(value.String())
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		buf.WriteString(strconv.FormatInt(value.Int(), 10))
		return nil
	case reflect.Int64:
		// Protobuf JSON encodes 64-bit integers as strings.
		buf.WriteByte('"')
		buf.WriteString(strconv.FormatInt(value.Int(), 10))
		buf.WriteByte('"')
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		buf.WriteString(strconv.FormatUint(value.Uint(), 10))
		return nil
	case reflect.Uint64:
		buf.WriteByte('"')
		buf.WriteString(strconv.FormatUint(value.Uint(), 10))
		buf.WriteByte('"')
		return nil
	case reflect.Float32, reflect.Float64:
		float := value.Float()
		switch {
		case math.IsNaN(float):
			buf.WriteString(`"NaN"`)
		case math.IsInf(float, 1):
			buf.WriteString(`"Infinity"`)
		case math.IsInf(float, -1):
			buf.WriteString(`"-Infinity"`)
		default:
			buf.WriteString(strconv.FormatFloat(float, 'g', -1, value.Type().Bits()))
		}
		return nil
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
			if value.Len() == 0 {
				buf.WriteString(`""`)
				return nil
			}
			encoded, err := json.Marshal(value.Bytes())
			if err != nil {
				return err
			}
			buf.Write(encoded)
			return nil
		}
		buf.WriteByte('[')
		for i := range value.Len() {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeProtoValue(buf, value.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("capsulad: JSON map key must be a string, got %s", value.Type().Key())
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, err := json.Marshal(key.String())
			if err != nil {
				return err
			}
			buf.Write(encoded)
			buf.WriteByte(':')
			if err := encodeProtoValue(buf, value.MapIndex(key)); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case reflect.Struct:
		return encodeProtoStruct(buf, value)
	default:
		return fmt.Errorf("capsulad: cannot encode %s as protobuf JSON", value.Type())
	}
}

func encodeProtoStruct(buf *bytes.Buffer, value reflect.Value) error {
	type field struct {
		name  string
		value reflect.Value
	}
	var fields []field

	var collect func(reflect.Value) error
	collect = func(structValue reflect.Value) error {
		typ := structValue.Type()
		for i := range typ.NumField() {
			info := typ.Field(i)
			if info.PkgPath != "" && !info.Anonymous {
				continue // unexported
			}
			tag := info.Tag.Get("json")
			name := info.Name
			opts := ""
			if tag != "" {
				parts := bytes.SplitN([]byte(tag), []byte(","), 2)
				name = string(parts[0])
				if len(parts) > 1 {
					opts = string(parts[1])
				}
			}
			if name == "-" {
				continue
			}
			fieldValue := structValue.Field(i)
			if info.Anonymous && tag == "" && fieldValue.Kind() == reflect.Struct {
				if err := collect(fieldValue); err != nil {
					return err
				}
				continue
			}
			if name == "" {
				name = lowerCamelName(info.Name)
			}
			if hasJSONOption(opts, "omitempty") && isEmptyProtoValue(fieldValue) {
				continue
			}
			fields = append(fields, field{name: name, value: fieldValue})
		}
		return nil
	}
	if err := collect(value); err != nil {
		return err
	}

	buf.WriteByte('{')
	written := 0
	for _, entry := range fields {
		if written > 0 {
			buf.WriteByte(',')
		}
		encoded, err := json.Marshal(entry.name)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		buf.WriteByte(':')
		if err := encodeProtoValue(buf, entry.value); err != nil {
			return err
		}
		written++
	}
	buf.WriteByte('}')
	return nil
}

func hasJSONOption(opts, option string) bool {
	for len(opts) > 0 {
		next := opts
		if idx := indexByte(opts, ','); idx >= 0 {
			next = opts[:idx]
			opts = opts[idx+1:]
		} else {
			opts = ""
		}
		if next == option {
			return true
		}
	}
	return false
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func isEmptyProtoValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool:
		return !value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return value.IsNil()
	}
	return false
}

func isNilable(kind reflect.Kind) bool {
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return true
	}
	return false
}

// unmarshalProtoJSON decodes canonical protobuf JSON, accepting 64-bit
// integers either quoted (canonical) or as numbers, and ignoring unknown
// fields for forward compatibility.
func unmarshalProtoJSON(data []byte, v any) error {
	if v == nil {
		return nil
	}
	target := reflect.ValueOf(v)
	if target.Kind() != reflect.Pointer || target.IsNil() {
		return fmt.Errorf("capsulad: decode target must be a non-nil pointer, got %T", v)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	return decodeProtoValue(raw, target.Elem())
}

func decodeProtoValue(raw any, target reflect.Value) error {
	if !target.IsValid() {
		return nil
	}
	if target.Kind() == reflect.Pointer {
		if raw == nil {
			target.SetZero()
			return nil
		}
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		return decodeProtoValue(raw, target.Elem())
	}
	if raw == nil {
		target.SetZero()
		return nil
	}
	if target.CanAddr() && target.Addr().Type().Implements(jsonUnmarshalerType) {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		return target.Addr().Interface().(json.Unmarshaler).UnmarshalJSON(encoded)
	}
	switch target.Kind() {
	case reflect.Interface:
		target.Set(reflect.ValueOf(raw))
		return nil
	case reflect.String:
		text, ok := raw.(string)
		if !ok {
			return typeMismatch(raw, target)
		}
		target.SetString(text)
		return nil
	case reflect.Bool:
		flag, ok := raw.(bool)
		if !ok {
			return typeMismatch(raw, target)
		}
		target.SetBool(flag)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number, err := protoInt(raw, target.Type().Bits())
		if err != nil {
			return err
		}
		if target.OverflowInt(number) {
			return fmt.Errorf("capsulad: integer %d overflows %s", number, target.Type())
		}
		target.SetInt(number)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		number, err := protoUint(raw, target.Type().Bits())
		if err != nil {
			return err
		}
		if target.OverflowUint(number) {
			return fmt.Errorf("capsulad: integer %d overflows %s", number, target.Type())
		}
		target.SetUint(number)
		return nil
	case reflect.Float32, reflect.Float64:
		number, err := protoFloat(raw)
		if err != nil {
			return err
		}
		target.SetFloat(number)
		return nil
	case reflect.Slice, reflect.Array:
		if target.Kind() == reflect.Slice && target.Type().Elem().Kind() == reflect.Uint8 {
			decoded, err := protoBytes(raw)
			if err != nil {
				return err
			}
			target.SetBytes(decoded)
			return nil
		}
		list, ok := raw.([]any)
		if !ok {
			return typeMismatch(raw, target)
		}
		if target.Kind() == reflect.Slice {
			target.Set(reflect.MakeSlice(target.Type(), len(list), len(list)))
		} else if len(list) > target.Len() {
			return fmt.Errorf("capsulad: array %s has %d elements, JSON has %d", target.Type(), target.Len(), len(list))
		}
		for i, item := range list {
			if err := decodeProtoValue(item, target.Index(i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		object, ok := raw.(map[string]any)
		if !ok {
			return typeMismatch(raw, target)
		}
		if target.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("capsulad: JSON map key must decode to a string, got %s", target.Type().Key())
		}
		created := reflect.MakeMapWithSize(target.Type(), len(object))
		for key, item := range object {
			value := reflect.New(target.Type().Elem()).Elem()
			if err := decodeProtoValue(item, value); err != nil {
				return err
			}
			created.SetMapIndex(reflect.ValueOf(key).Convert(target.Type().Key()), value)
		}
		target.Set(created)
		return nil
	case reflect.Struct:
		object, ok := raw.(map[string]any)
		if !ok {
			return typeMismatch(raw, target)
		}
		return decodeProtoStruct(object, target)
	default:
		return fmt.Errorf("capsulad: cannot decode %s from protobuf JSON", target.Type())
	}
}

func decodeProtoStruct(object map[string]any, target reflect.Value) error {
	typ := target.Type()
	for i := range typ.NumField() {
		info := typ.Field(i)
		if info.PkgPath != "" && !info.Anonymous {
			continue
		}
		fieldValue := target.Field(i)
		if !fieldValue.CanSet() {
			continue
		}
		tag := info.Tag.Get("json")
		name := info.Name
		if tag != "" {
			if idx := indexByte(tag, ','); idx >= 0 {
				name = tag[:idx]
			} else {
				name = tag
			}
		}
		if name == "-" {
			continue
		}
		if info.Anonymous && tag == "" && fieldValue.Kind() == reflect.Struct {
			if err := decodeProtoStruct(object, fieldValue); err != nil {
				return err
			}
			continue
		}
		if name == "" {
			name = lowerCamelName(info.Name)
		}
		raw, ok := object[name]
		if !ok {
			continue
		}
		if err := decodeProtoValue(raw, fieldValue); err != nil {
			return fmt.Errorf("capsulad: field %s: %w", info.Name, err)
		}
	}
	return nil
}

func protoInt(raw any, bits int) (int64, error) {
	switch value := raw.(type) {
	case json.Number:
		return strconv.ParseInt(value.String(), 10, bits)
	case string:
		return strconv.ParseInt(value, 10, bits)
	case float64:
		return int64(value), nil
	}
	return 0, fmt.Errorf("capsulad: expected integer, got %T", raw)
}

func protoUint(raw any, bits int) (uint64, error) {
	switch value := raw.(type) {
	case json.Number:
		return strconv.ParseUint(value.String(), 10, bits)
	case string:
		return strconv.ParseUint(value, 10, bits)
	case float64:
		return uint64(value), nil
	}
	return 0, fmt.Errorf("capsulad: expected unsigned integer, got %T", raw)
}

func protoFloat(raw any) (float64, error) {
	switch value := raw.(type) {
	case json.Number:
		return strconv.ParseFloat(value.String(), 64)
	case string:
		switch value {
		case "NaN":
			return math.NaN(), nil
		case "Infinity":
			return math.Inf(1), nil
		case "-Infinity":
			return math.Inf(-1), nil
		}
		return strconv.ParseFloat(value, 64)
	case float64:
		return value, nil
	}
	return 0, fmt.Errorf("capsulad: expected number, got %T", raw)
}

func protoBytes(raw any) ([]byte, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("capsulad: expected base64 bytes, got %T", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("capsulad: invalid base64 bytes: %w", err)
	}
	return decoded, nil
}

func typeMismatch(raw any, target reflect.Value) error {
	return fmt.Errorf("capsulad: cannot decode JSON %T into %s", raw, target.Type())
}

func lowerCamelName(name string) string {
	if name == "" {
		return name
	}
	upper := 0
	for upper < len(name) && name[upper] >= 'A' && name[upper] <= 'Z' {
		upper++
	}
	switch {
	case upper == 0:
		return name
	case upper == len(name):
		return strings.ToLower(name)
	case upper == 1:
		return string(name[0]+('a'-'A')) + name[1:]
	default:
		return strings.ToLower(name[:upper-1]) + name[upper-1:]
	}
}
