package runtimehttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	coreerrors "github.com/nucleuskit/nucleus/core/errors"
)

type RequestDecoder interface {
	DecodeHTTPRequest(*http.Request, any) error
}

type RequestDecoderFunc func(*http.Request, any) error

type JSONDecoder struct{}

func (fn RequestDecoderFunc) DecodeHTTPRequest(request *http.Request, target any) error {
	if fn == nil {
		return nil
	}
	return fn(request, target)
}

func (JSONDecoder) DecodeHTTPRequest(request *http.Request, target any) error {
	return BindJSON(request, target)
}

func BindQueryParam(request *http.Request, name string) (string, error) {
	value := request.URL.Query().Get(name)
	if value == "" {
		return "", invalidArgument("missing query parameter %q", name)
	}
	return value, nil
}

func BindPathParam(request *http.Request, name string) (string, error) {
	value := request.PathValue(name)
	if value == "" {
		return "", invalidArgument("missing path parameter %q", name)
	}
	return value, nil
}

func BindJSON(request *http.Request, target any) error {
	if request.Body == nil {
		return invalidArgument("missing request body")
	}
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		return coreerrors.Wrap(coreerrors.CodeInvalidArgument, "invalid request body", err)
	}
	return nil
}

func BindQuery(request *http.Request, target any) error {
	if err := bindValues(request.URL.Query(), "query", target); err != nil {
		return coreerrors.Wrap(coreerrors.CodeInvalidArgument, "invalid query parameters", err)
	}
	return nil
}

func BindForm(request *http.Request, target any) error {
	if err := request.ParseForm(); err != nil {
		return coreerrors.Wrap(coreerrors.CodeInvalidArgument, "invalid form body", err)
	}
	if err := bindValues(request.PostForm, "form", target); err != nil {
		return coreerrors.Wrap(coreerrors.CodeInvalidArgument, "invalid form body", err)
	}
	return nil
}

func invalidArgument(format string, args ...any) error {
	return coreerrors.New(coreerrors.CodeInvalidArgument, fmt.Sprintf(format, args...))
}

func bindValues(values url.Values, tagName string, target any) error {
	if target == nil {
		return fmt.Errorf("nil target")
	}
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("target must be a non-nil pointer")
	}
	value = value.Elem()
	if value.Kind() != reflect.Struct {
		return fmt.Errorf("target must point to a struct")
	}
	targetType := value.Type()
	for i := 0; i < value.NumField(); i++ {
		field := targetType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, ok := bindTagName(field.Tag.Get(tagName))
		if !ok {
			continue
		}
		entries, exists := values[name]
		if !exists || len(entries) == 0 {
			continue
		}
		if err := setValue(value.Field(i), entries); err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}
	}
	return nil
}

func bindTagName(tag string) (string, bool) {
	if tag == "" || tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return "", false
	}
	return name, true
}

func setValue(target reflect.Value, entries []string) error {
	if !target.CanSet() {
		return fmt.Errorf("cannot set value")
	}
	for target.Kind() == reflect.Pointer {
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		target = target.Elem()
	}
	if target.Kind() == reflect.Slice {
		slice := reflect.MakeSlice(target.Type(), 0, len(entries))
		for _, entry := range entries {
			element := reflect.New(target.Type().Elem()).Elem()
			if err := setScalar(element, entry); err != nil {
				return err
			}
			slice = reflect.Append(slice, element)
		}
		target.Set(slice)
		return nil
	}
	return setScalar(target, entries[len(entries)-1])
}

func setScalar(target reflect.Value, entry string) error {
	switch target.Kind() {
	case reflect.String:
		target.SetString(entry)
	case reflect.Bool:
		value, err := strconv.ParseBool(entry)
		if err != nil {
			return err
		}
		target.SetBool(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(entry, 10, target.Type().Bits())
		if err != nil {
			return err
		}
		target.SetInt(value)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value, err := strconv.ParseUint(entry, 10, target.Type().Bits())
		if err != nil {
			return err
		}
		target.SetUint(value)
	default:
		return fmt.Errorf("unsupported field type %s", target.Type())
	}
	return nil
}
