// Copyright (c) 2017-2022 Snowflake Computing Inc. All rights reserved.

package gosnowflake

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/compute"
	"github.com/apache/arrow/go/v15/arrow/decimal128"
	"github.com/apache/arrow/go/v15/arrow/memory"
)

const format = "2006-01-02 15:04:05.999999999"

type timezoneType int

const (
	// TimestampNTZType denotes a NTZ timezoneType for array binds
	TimestampNTZType timezoneType = iota
	// TimestampLTZType denotes a LTZ timezoneType for array binds
	TimestampLTZType
	// TimestampTZType denotes a TZ timezoneType for array binds
	TimestampTZType
	// DateType denotes a date type for array binds
	DateType
	// TimeType denotes a time type for array binds
	TimeType
)

type snowflakeArrowBatchesTimestampOption int

const (
	// UseNanosecondTimestamp uses arrow.Timestamp in nanosecond precision, could cause ErrTooHighTimestampPrecision if arrow.Timestamp cannot fit original timestamp values.
	UseNanosecondTimestamp snowflakeArrowBatchesTimestampOption = iota
	// UseMicrosecondTimestamp uses arrow.Timestamp in microsecond precision
	UseMicrosecondTimestamp
	// UseMillisecondTimestamp uses arrow.Timestamp in millisecond precision
	UseMillisecondTimestamp
	// UseSecondTimestamp uses arrow.Timestamp in second precision
	UseSecondTimestamp
	// UseOriginalTimestamp uses original timestamp struct returned by Snowflake. It can be used in case arrow.Timestamp cannot fit original timestamp values.
	UseOriginalTimestamp
)

type interfaceArrayBinding struct {
	hasTimezone       bool
	tzType            timezoneType
	timezoneTypeArray interface{}
}

func isInterfaceArrayBinding(t interface{}) bool {
	switch t.(type) {
	case interfaceArrayBinding:
		return true
	case *interfaceArrayBinding:
		return true
	default:
		return false
	}
}

// goTypeToSnowflake translates Go data type to Snowflake data type.
func goTypeToSnowflake(v driver.Value, tsmode snowflakeType) snowflakeType {
	switch t := v.(type) {
	case int64, sql.NullInt64:
		return fixedType
	case float64, sql.NullFloat64:
		return realType
	case bool, sql.NullBool:
		return booleanType
	case string, sql.NullString:
		return textType
	case []byte:
		if tsmode == binaryType {
			return binaryType // may be redundant but ensures BINARY type
		}
		if t == nil {
			return nullType // invalid byte array. won't take as BINARY
		}
		if len(t) != 1 {
			return unSupportedType
		}
		if _, err := dataTypeMode(t); err != nil {
			return unSupportedType
		}
		return changeType
	case time.Time, sql.NullTime:
		return tsmode
	}
	if supportedArrayBind(&driver.NamedValue{Value: v}) {
		return sliceType
	}
	return unSupportedType
}

// snowflakeTypeToGo translates Snowflake data type to Go data type.
func snowflakeTypeToGo(ctx context.Context, dbtype snowflakeType, scale int64, fields []fieldMetadata) reflect.Type {
	switch dbtype {
	case fixedType:
		if scale == 0 {
			return reflect.TypeOf(int64(0))
		}
		return reflect.TypeOf(float64(0))
	case realType:
		return reflect.TypeOf(float64(0))
	case textType, variantType:
		return reflect.TypeOf("")
	case dateType, timeType, timestampLtzType, timestampNtzType, timestampTzType:
		return reflect.TypeOf(time.Now())
	case binaryType:
		return reflect.TypeOf([]byte{})
	case booleanType:
		return reflect.TypeOf(true)
	case objectType:
		if len(fields) > 0 {
			return reflect.TypeOf(ObjectType{})
		}
		return reflect.TypeOf("")
	case arrayType:
		if len(fields) == 0 {
			return reflect.TypeOf("")
		}
		if len(fields) != 1 {
			logger.WithContext(ctx).Warn("Unexpected fields number: " + strconv.Itoa(len(fields)))
			return reflect.TypeOf("")
		}
		switch getSnowflakeType(fields[0].Type) {
		case fixedType:
			if fields[0].Scale == 0 && higherPrecisionEnabled(ctx) {
				return reflect.TypeOf([]*big.Int{})
			} else if fields[0].Scale == 0 && !higherPrecisionEnabled(ctx) {
				return reflect.TypeOf([]int64{})
			} else if fields[0].Scale != 0 && higherPrecisionEnabled(ctx) {
				return reflect.TypeOf([]*big.Float{})
			}
			return reflect.TypeOf([]float64{})
		case realType:
			return reflect.TypeOf([]float64{})
		case textType:
			return reflect.TypeOf([]string{})
		case dateType, timeType, timestampLtzType, timestampNtzType, timestampTzType:
			return reflect.TypeOf([]time.Time{})
		case booleanType:
			return reflect.TypeOf([]bool{})
		case binaryType:
			return reflect.TypeOf([][]byte{})
		case objectType:
			return reflect.TypeOf([]ObjectType{})
		}
		return nil
	case mapType:
		switch getSnowflakeType(fields[0].Type) {
		case textType:
			return snowflakeTypeToGoForMaps[string](ctx, fields[1])
		case fixedType:
			return snowflakeTypeToGoForMaps[int64](ctx, fields[1])
		}
		return reflect.TypeOf(map[any]any{})
	}
	logger.WithContext(ctx).Errorf("unsupported dbtype is specified. %v", dbtype)
	return reflect.TypeOf("")
}

func snowflakeTypeToGoForMaps[K comparable](ctx context.Context, valueMetadata fieldMetadata) reflect.Type {
	switch getSnowflakeType(valueMetadata.Type) {
	case textType:
		return reflect.TypeOf(map[K]string{})
	case fixedType:
		if higherPrecisionEnabled(ctx) && valueMetadata.Scale == 0 {
			return reflect.TypeOf(map[K]*big.Int{})
		} else if higherPrecisionEnabled(ctx) && valueMetadata.Scale != 0 {
			return reflect.TypeOf(map[K]*big.Float{})
		} else if !higherPrecisionEnabled(ctx) && valueMetadata.Scale == 0 {
			return reflect.TypeOf(map[K]int64{})
		} else {
			return reflect.TypeOf(map[K]float64{})
		}
	case realType:
		return reflect.TypeOf(map[K]float64{})
	case booleanType:
		return reflect.TypeOf(map[K]bool{})
	case binaryType:
		return reflect.TypeOf(map[K][]byte{})
	case timeType, dateType, timestampTzType, timestampNtzType, timestampLtzType:
		return reflect.TypeOf(map[K]time.Time{})
	}
	logger.WithContext(ctx).Errorf("unsupported dbtype is specified for map value")
	return reflect.TypeOf("")
}

// valueToString converts arbitrary golang type to a string. This is mainly used in binding data with placeholders
// in queries.
func valueToString(v driver.Value, tsmode snowflakeType) (*string, error) {
	logger.Debugf("TYPE: %v, %v", reflect.TypeOf(v), reflect.ValueOf(v))
	if v == nil {
		return nil, nil
	}
	v1 := reflect.ValueOf(v)
	switch v1.Kind() {
	case reflect.Bool:
		s := strconv.FormatBool(v1.Bool())
		return &s, nil
	case reflect.Int64:
		s := strconv.FormatInt(v1.Int(), 10)
		return &s, nil
	case reflect.Float64:
		s := strconv.FormatFloat(v1.Float(), 'g', -1, 32)
		return &s, nil
	case reflect.String:
		s := v1.String()
		return &s, nil
	case reflect.Slice, reflect.Map:
		if v1.IsNil() {
			return nil, nil
		}
		if bd, ok := v.([]byte); ok {
			if tsmode == binaryType {
				s := hex.EncodeToString(bd)
				return &s, nil
			}
		}
		// TODO: is this good enough?
		s := v1.String()
		return &s, nil
	case reflect.Struct:
		switch typedVal := v.(type) {
		case time.Time:
			return timeTypeValueToString(typedVal, tsmode)
		case sql.NullTime:
			if !typedVal.Valid {
				return nil, nil
			}
			return timeTypeValueToString(typedVal.Time, tsmode)
		case sql.NullBool:
			if !typedVal.Valid {
				return nil, nil
			}
			s := strconv.FormatBool(typedVal.Bool)
			return &s, nil
		case sql.NullInt64:
			if !typedVal.Valid {
				return nil, nil
			}
			s := strconv.FormatInt(typedVal.Int64, 10)
			return &s, nil
		case sql.NullFloat64:
			if !typedVal.Valid {
				return nil, nil
			}
			s := strconv.FormatFloat(typedVal.Float64, 'g', -1, 32)
			return &s, nil
		case sql.NullString:
			if !typedVal.Valid {
				return nil, nil
			}
			return &typedVal.String, nil
		}
	}
	return nil, fmt.Errorf("unsupported type: %v", v1.Kind())
}

func timeTypeValueToString(tm time.Time, tsmode snowflakeType) (*string, error) {
	switch tsmode {
	case dateType:
		_, offset := tm.Zone()
		tm = tm.Add(time.Second * time.Duration(offset))
		s := strconv.FormatInt(tm.Unix()*1000, 10)
		return &s, nil
	case timeType:
		s := fmt.Sprintf("%d",
			(tm.Hour()*3600+tm.Minute()*60+tm.Second())*1e9+tm.Nanosecond())
		return &s, nil
	case timestampNtzType, timestampLtzType:
		unixTime, _ := new(big.Int).SetString(fmt.Sprintf("%d", tm.Unix()), 10)
		m, _ := new(big.Int).SetString(strconv.FormatInt(1e9, 10), 10)
		unixTime.Mul(unixTime, m)
		tmNanos, _ := new(big.Int).SetString(fmt.Sprintf("%d", tm.Nanosecond()), 10)
		s := unixTime.Add(unixTime, tmNanos).String()
		return &s, nil
	case timestampTzType:
		_, offset := tm.Zone()
		s := fmt.Sprintf("%v %v", tm.UnixNano(), offset/60+1440)
		return &s, nil
	}
	return nil, fmt.Errorf("unsupported time type: %v", tsmode)
}

// extractTimestamp extracts the internal timestamp data to epoch time in seconds and milliseconds
func extractTimestamp(srcValue *string) (sec int64, nsec int64, err error) {
	logger.Debugf("SRC: %v", srcValue)
	var i int
	for i = 0; i < len(*srcValue); i++ {
		if (*srcValue)[i] == '.' {
			sec, err = strconv.ParseInt((*srcValue)[0:i], 10, 64)
			if err != nil {
				return 0, 0, err
			}
			break
		}
	}
	if i == len(*srcValue) {
		// no fraction
		sec, err = strconv.ParseInt(*srcValue, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		nsec = 0
	} else {
		s := (*srcValue)[i+1:]
		nsec, err = strconv.ParseInt(s+strings.Repeat("0", 9-len(s)), 10, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	logger.Infof("sec: %v, nsec: %v", sec, nsec)
	return sec, nsec, nil
}

// stringToValue converts a pointer of string data to an arbitrary golang variable
// This is mainly used in fetching data.
func stringToValue(ctx context.Context, dest *driver.Value, srcColumnMeta execResponseRowType, srcValue *string, loc *time.Location, params map[string]*string) error {
	if srcValue == nil {
		logger.Debugf("snowflake data type: %v, raw value: nil", srcColumnMeta.Type)
		*dest = nil
		return nil
	}
	logger.Debugf("snowflake data type: %v, raw value: %v", srcColumnMeta.Type, *srcValue)
	switch srcColumnMeta.Type {
	case "object":
		if len(srcColumnMeta.Fields) == 0 {
			// semistructured type without schema
			*dest = *srcValue
			return nil
		}
		m := make(map[string]any)
		decoder := decoderWithNumbersAsStrings(srcValue)
		if err := decoder.Decode(&m); err != nil {
			return err
		}
		v, err := buildStructuredTypeRecursive(ctx, m, srcColumnMeta.Fields, params)
		if err != nil {
			return err
		}
		*dest = v
		return nil
	case "text", "fixed", "real", "variant":
		*dest = *srcValue
		return nil
	case "date":
		v, err := strconv.ParseInt(*srcValue, 10, 64)
		if err != nil {
			return err
		}
		*dest = time.Unix(v*86400, 0).UTC()
		return nil
	case "time":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		t0 := time.Time{}
		*dest = t0.Add(time.Duration(sec*1e9 + nsec))
		return nil
	case "timestamp_ntz":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		*dest = time.Unix(sec, nsec).UTC()
		return nil
	case "timestamp_ltz":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		if loc == nil {
			loc = time.Now().Location()
		}
		*dest = time.Unix(sec, nsec).In(loc)
		return nil
	case "timestamp_tz":
		logger.Debugf("tz: %v", *srcValue)

		tm := strings.Split(*srcValue, " ")
		if len(tm) != 2 {
			return &SnowflakeError{
				Number:   ErrInvalidTimestampTz,
				SQLState: SQLStateInvalidDataTimeFormat,
				Message:  fmt.Sprintf("invalid TIMESTAMP_TZ data. The value doesn't consist of two numeric values separated by a space: %v", *srcValue),
			}
		}
		sec, nsec, err := extractTimestamp(&tm[0])
		if err != nil {
			return err
		}
		offset, err := strconv.ParseInt(tm[1], 10, 64)
		if err != nil {
			return &SnowflakeError{
				Number:   ErrInvalidTimestampTz,
				SQLState: SQLStateInvalidDataTimeFormat,
				Message:  fmt.Sprintf("invalid TIMESTAMP_TZ data. The offset value is not integer: %v", tm[1]),
			}
		}
		loc := Location(int(offset) - 1440)
		tt := time.Unix(sec, nsec)
		*dest = tt.In(loc)
		return nil
	case "binary":
		b, err := hex.DecodeString(*srcValue)
		if err != nil {
			return &SnowflakeError{
				Number:   ErrInvalidBinaryHexForm,
				SQLState: SQLStateNumericValueOutOfRange,
				Message:  err.Error(),
			}
		}
		*dest = b
		return nil
	case "array":
		if len(srcColumnMeta.Fields) == 0 {
			*dest = *srcValue
			return nil
		}
		if len(srcColumnMeta.Fields) > 1 {
			return errors.New("got more than one field for array")
		}
		var arr []any
		decoder := decoderWithNumbersAsStrings(srcValue)
		if err := decoder.Decode(&arr); err != nil {
			return err
		}
		v, err := buildStructuredArray(ctx, srcColumnMeta.Fields[0], arr, params)
		if err != nil {
			return err
		}
		*dest = v
		return nil
	case "map":
		var err error
		*dest, err = jsonToMap(ctx, srcColumnMeta.Fields[0], srcColumnMeta.Fields[1], *srcValue, params)
		return err
	}
	*dest = *srcValue
	return nil
}

func jsonToMap(ctx context.Context, keyMetadata, valueMetadata fieldMetadata, srcValue string, params map[string]*string) (snowflakeValue, error) {
	switch keyMetadata.Type {
	case "text":
		var m map[string]any
		decoder := decoderWithNumbersAsStrings(&srcValue)
		err := decoder.Decode(&m)
		if err != nil {
			return nil, err
		}
		// returning snowflakeValue of complex types does not work with generics
		if valueMetadata.Type == "object" {
			res := make(map[string]*structuredType)
			for k, v := range m {
				res[k] = buildStructuredTypeFromMap(v.(map[string]any), valueMetadata.Fields, params)
			}
			return res, nil
		}
		return jsonToMapWithKeyType[string](ctx, valueMetadata, m, params)
	case "fixed":
		var m map[int64]any
		decoder := decoderWithNumbersAsStrings(&srcValue)
		err := decoder.Decode(&m)
		if err != nil {
			return nil, err
		}
		if valueMetadata.Type == "object" {
			res := make(map[int64]*structuredType)
			for k, v := range m {
				res[k] = buildStructuredTypeFromMap(v.(map[string]any), valueMetadata.Fields, params)
			}
			return res, nil
		}
		return jsonToMapWithKeyType[int64](ctx, valueMetadata, m, params)
	default:
		return nil, fmt.Errorf("unsupported map key type: %v", keyMetadata.Type)
	}
}

func jsonToMapWithKeyType[K comparable](ctx context.Context, valueMetadata fieldMetadata, m map[K]any, params map[string]*string) (snowflakeValue, error) {
	mapValuesNullableEnabled := mapValuesNullableEnabled(ctx)
	switch valueMetadata.Type {
	case "text":
		return buildMapValues[K, sql.NullString, string](mapValuesNullableEnabled, m, func(v any) (string, error) {
			return v.(string), nil
		}, func(v any) (sql.NullString, error) {
			return sql.NullString{Valid: v != nil, String: ifNotNullOrDefault(v, "")}, nil
		})
	case "boolean":
		return buildMapValues[K, sql.NullBool, bool](mapValuesNullableEnabled, m, func(v any) (bool, error) {
			return v.(bool), nil
		}, func(v any) (sql.NullBool, error) {
			return sql.NullBool{Valid: v != nil, Bool: ifNotNullOrDefault(v, false)}, nil
		})
	case "fixed":
		if valueMetadata.Scale == 0 {
			return buildMapValues[K, sql.NullInt64, int64](mapValuesNullableEnabled, m, func(v any) (int64, error) {
				return strconv.ParseInt(string(v.(json.Number)), 10, 64)
			}, func(v any) (sql.NullInt64, error) {
				if v != nil {
					i64, err := strconv.ParseInt(string(v.(json.Number)), 10, 64)
					if err != nil {
						return sql.NullInt64{}, err
					}
					return sql.NullInt64{Valid: true, Int64: i64}, nil
				}
				return sql.NullInt64{Valid: false}, nil
			})
		}
		return buildMapValues[K, sql.NullFloat64, float64](mapValuesNullableEnabled, m, func(v any) (float64, error) {
			return strconv.ParseFloat(string(v.(json.Number)), 64)
		}, func(v any) (sql.NullFloat64, error) {
			if v != nil {
				f64, err := strconv.ParseFloat(string(v.(json.Number)), 64)
				if err != nil {
					return sql.NullFloat64{}, err
				}
				return sql.NullFloat64{Valid: true, Float64: f64}, nil
			}
			return sql.NullFloat64{Valid: false}, nil
		})
	case "real":
		return buildMapValues[K, sql.NullFloat64, float64](mapValuesNullableEnabled, m, func(v any) (float64, error) {
			return strconv.ParseFloat(string(v.(json.Number)), 64)
		}, func(v any) (sql.NullFloat64, error) {
			if v != nil {
				f64, err := strconv.ParseFloat(string(v.(json.Number)), 64)
				if err != nil {
					return sql.NullFloat64{}, err
				}
				return sql.NullFloat64{Valid: true, Float64: f64}, nil
			}
			return sql.NullFloat64{Valid: false}, nil
		})
	case "binary":
		return buildMapValues[K, []byte, []byte](mapValuesNullableEnabled, m, func(v any) ([]byte, error) {
			return hex.DecodeString(v.(string))
		}, func(v any) ([]byte, error) {
			if v == nil {
				return nil, nil
			}
			return hex.DecodeString(v.(string))
		})
	case "date", "time", "timestamp_tz", "timestamp_ltz", "timestamp_ntz":
		return buildMapValues[K, sql.NullTime, time.Time](mapValuesNullableEnabled, m, func(v any) (time.Time, error) {
			sfFormat, err := dateTimeFormatByType(valueMetadata.Type, params)
			if err != nil {
				return time.Time{}, err
			}
			goFormat, err := snowflakeFormatToGoFormat(sfFormat)
			if err != nil {
				return time.Time{}, err
			}
			return time.Parse(goFormat, v.(string))
		}, func(v any) (sql.NullTime, error) {
			if v == nil {
				return sql.NullTime{Valid: false}, nil
			}
			sfFormat, err := dateTimeFormatByType(valueMetadata.Type, params)
			if err != nil {
				return sql.NullTime{}, err
			}
			goFormat, err := snowflakeFormatToGoFormat(sfFormat)
			if err != nil {
				return sql.NullTime{}, err
			}
			time, err := time.Parse(goFormat, v.(string))
			if err != nil {
				return sql.NullTime{}, err
			}
			return sql.NullTime{Valid: true, Time: time}, nil
		})
	case "array":
		arrayMetadata := valueMetadata.Fields[0]
		switch arrayMetadata.Type {
		case "text":
			return buildArrayFromMap[K, string](ctx, arrayMetadata, m, params)
		case "fixed":
			if arrayMetadata.Scale == 0 {
				return buildArrayFromMap[K, int64](ctx, arrayMetadata, m, params)
			}
			return buildArrayFromMap[K, float64](ctx, arrayMetadata, m, params)
		case "real":
			return buildArrayFromMap[K, float64](ctx, arrayMetadata, m, params)
		case "binary":
			return buildArrayFromMap[K, []byte](ctx, arrayMetadata, m, params)
		case "boolean":
			return buildArrayFromMap[K, bool](ctx, arrayMetadata, m, params)
		case "date", "time", "timestamp_ltz", "timestamp_tz", "timestamp_ntz":
			return buildArrayFromMap[K, time.Time](ctx, arrayMetadata, m, params)
		}
	}
	return nil, fmt.Errorf("unsupported map value type: %v", valueMetadata.Type)
}

func buildArrayFromMap[K comparable, V any](ctx context.Context, valueMetadata fieldMetadata, m map[K]any, params map[string]*string) (snowflakeValue, error) {
	res := make(map[K][]V)
	for k, v := range m {
		structuredArray, err := buildStructuredArray(ctx, valueMetadata, v.([]any), params)
		if err != nil {
			return nil, err
		}
		res[k] = structuredArray.([]V)
	}
	return res, nil
}

func buildStructuredTypeFromMap(values map[string]any, fieldMetadata []fieldMetadata, params map[string]*string) *structuredType {
	return &structuredType{
		values:        values,
		params:        params,
		fieldMetadata: fieldMetadata,
	}
}

func ifNotNullOrDefault[T any](t any, def T) T {
	if t == nil {
		return def
	}
	return t.(T)
}

func buildMapValues[K comparable, Vnullable any, VnotNullable any](mapValuesNullableEnabled bool, m map[K]any, buildNotNullable func(v any) (VnotNullable, error), buildNullable func(v any) (Vnullable, error)) (snowflakeValue, error) {
	var err error
	if mapValuesNullableEnabled {
		result := make(map[K]Vnullable, len(m))
		for k, v := range m {
			if result[k], err = buildNullable(v); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	result := make(map[K]VnotNullable, len(m))
	for k, v := range m {
		if result[k], err = buildNotNullable(v); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func buildStructuredArray(ctx context.Context, fieldMetadata fieldMetadata, srcValue []any, params map[string]*string) (any, error) {
	switch fieldMetadata.Type {
	case "text":
		return copyArrayAndConvert[string](srcValue, func(input any) (string, error) {
			return input.(string), nil
		})
	case "fixed":
		if fieldMetadata.Scale == 0 {
			return copyArrayAndConvert[int64](srcValue, func(input any) (int64, error) {
				return strconv.ParseInt(string(input.(json.Number)), 10, 64)
			})
		}
		return copyArrayAndConvert[float64](srcValue, func(input any) (float64, error) {
			return strconv.ParseFloat(string(input.(json.Number)), 64)
		})
	case "real":
		return copyArrayAndConvert[float64](srcValue, func(input any) (float64, error) {
			return strconv.ParseFloat(string(input.(json.Number)), 64)
		})
	case "time", "date", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
		return copyArrayAndConvert[time.Time](srcValue, func(input any) (time.Time, error) {
			sfFormat, err := dateTimeFormatByType(fieldMetadata.Type, params)
			if err != nil {
				return time.Time{}, err
			}
			goFormat, err := snowflakeFormatToGoFormat(sfFormat)
			if err != nil {
				return time.Time{}, err
			}
			return time.Parse(goFormat, input.(string))
		})
	case "boolean":
		return copyArrayAndConvert[bool](srcValue, func(input any) (bool, error) {
			return input.(bool), nil
		})
	case "binary":
		return copyArrayAndConvert[[]byte](srcValue, func(input any) ([]byte, error) {
			return hex.DecodeString(input.(string))
		})
	case "object":
		return copyArrayAndConvert[*structuredType](srcValue, func(input any) (*structuredType, error) {
			return buildStructuredTypeRecursive(ctx, input.(map[string]any), fieldMetadata.Fields, params)
		})
	case "array":
		switch fieldMetadata.Fields[0].Type {
		case "text":
			return buildStructuredArrayRecursive[string](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "fixed":
			if fieldMetadata.Fields[0].Scale == 0 {
				return buildStructuredArrayRecursive[int64](ctx, fieldMetadata.Fields[0], srcValue, params)
			}
			return buildStructuredArrayRecursive[float64](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "real":
			return buildStructuredArrayRecursive[float64](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "boolean":
			return buildStructuredArrayRecursive[bool](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "binary":
			return buildStructuredArrayRecursive[[]byte](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "date", "time", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
			return buildStructuredArrayRecursive[time.Time](ctx, fieldMetadata.Fields[0], srcValue, params)
		}
	}
	return srcValue, nil
}

func buildStructuredArrayRecursive[T any](ctx context.Context, fieldMetadata fieldMetadata, srcValue []any, params map[string]*string) ([][]T, error) {
	arr := make([][]T, len(srcValue))
	for i, v := range srcValue {
		structuredArray, err := buildStructuredArray(ctx, fieldMetadata, v.([]any), params)
		if err != nil {
			return nil, err
		}
		arr[i] = structuredArray.([]T)
	}
	return arr, nil
}

func copyArrayAndConvert[T any](input []any, convertFunc func(input any) (T, error)) ([]T, error) {
	var err error
	output := make([]T, len(input))
	for i, s := range input {
		if output[i], err = convertFunc(s); err != nil {
			return nil, err
		}
	}
	return output, nil
}

func buildStructuredTypeRecursive(ctx context.Context, m map[string]any, fields []fieldMetadata, params map[string]*string) (*structuredType, error) {
	var err error
	for _, fm := range fields {
		if fm.Type == "array" && m[fm.Name] != nil {
			if m[fm.Name], err = buildStructuredArray(ctx, fm.Fields[0], m[fm.Name].([]any), params); err != nil {
				return nil, err
			}
		} else if fm.Type == "map" && m[fm.Name] != nil {
			if m[fm.Name], err = jsonToMapWithKeyType(ctx, fm.Fields[1], m[fm.Name].(map[string]any), params); err != nil {
				return nil, err
			}
		} else if fm.Type == "object" && m[fm.Name] != nil {
			if m[fm.Name], err = buildStructuredTypeRecursive(ctx, m[fm.Name].(map[string]any), fm.Fields, params); err != nil {
				return nil, err
			}
		}
	}
	return &structuredType{
		values:        m,
		fieldMetadata: fields,
		params:        params,
	}, nil
}

var decimalShift = new(big.Int).Exp(big.NewInt(2), big.NewInt(64), nil)

func intToBigFloat(val int64, scale int64) *big.Float {
	f := new(big.Float).SetInt64(val)
	s := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil))
	return new(big.Float).Quo(f, s)
}

func decimalToBigInt(num decimal128.Num) *big.Int {
	high := new(big.Int).SetInt64(num.HighBits())
	low := new(big.Int).SetUint64(num.LowBits())
	return new(big.Int).Add(new(big.Int).Mul(high, decimalShift), low)
}

func decimalToBigFloat(num decimal128.Num, scale int64) *big.Float {
	f := new(big.Float).SetInt(decimalToBigInt(num))
	s := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil))
	return new(big.Float).Quo(f, s)
}

// ArrowSnowflakeTimestampToTime converts original timestamp returned by Snowflake to time.Time
func (rb *ArrowBatch) ArrowSnowflakeTimestampToTime(rec arrow.Record, colIdx int, recIdx int) *time.Time {
	scale := int(rb.scd.RowSet.RowType[colIdx].Scale)
	dbType := rb.scd.RowSet.RowType[colIdx].Type
	return arrowSnowflakeTimestampToTime(rec.Column(colIdx), getSnowflakeType(dbType), scale, recIdx, rb.loc)
}

func arrowSnowflakeTimestampToTime(
	column arrow.Array,
	sfType snowflakeType,
	scale int,
	recIdx int,
	loc *time.Location) *time.Time {

	if column.IsNull(recIdx) {
		return nil
	}
	var ret time.Time
	switch sfType {
	case timestampNtzType:
		if column.DataType().ID() == arrow.STRUCT {
			structData := column.(*array.Struct)
			epoch := structData.Field(0).(*array.Int64).Int64Values()
			fraction := structData.Field(1).(*array.Int32).Int32Values()
			ret = time.Unix(epoch[recIdx], int64(fraction[recIdx])).UTC()
		} else {
			intData := column.(*array.Int64)
			value := intData.Value(recIdx)
			epoch := extractEpoch(value, scale)
			fraction := extractFraction(value, scale)
			ret = time.Unix(epoch, fraction).UTC()
		}
	case timestampLtzType:
		if column.DataType().ID() == arrow.STRUCT {
			structData := column.(*array.Struct)
			epoch := structData.Field(0).(*array.Int64).Int64Values()
			fraction := structData.Field(1).(*array.Int32).Int32Values()
			ret = time.Unix(epoch[recIdx], int64(fraction[recIdx])).In(loc)
		} else {
			intData := column.(*array.Int64)
			value := intData.Value(recIdx)
			epoch := extractEpoch(value, scale)
			fraction := extractFraction(value, scale)
			ret = time.Unix(epoch, fraction).In(loc)
		}
	case timestampTzType:
		structData := column.(*array.Struct)
		if structData.NumField() == 2 {
			value := structData.Field(0).(*array.Int64).Int64Values()
			timezone := structData.Field(1).(*array.Int32).Int32Values()
			epoch := extractEpoch(value[recIdx], scale)
			fraction := extractFraction(value[recIdx], scale)
			locTz := Location(int(timezone[recIdx]) - 1440)
			ret = time.Unix(epoch, fraction).In(locTz)
		} else {
			epoch := structData.Field(0).(*array.Int64).Int64Values()
			fraction := structData.Field(1).(*array.Int32).Int32Values()
			timezone := structData.Field(2).(*array.Int32).Int32Values()
			locTz := Location(int(timezone[recIdx]) - 1440)
			ret = time.Unix(epoch[recIdx], int64(fraction[recIdx])).In(locTz)
		}
	}
	return &ret
}

func extractEpoch(value int64, scale int) int64 {
	return value / int64(math.Pow10(scale))
}

func extractFraction(value int64, scale int) int64 {
	return (value % int64(math.Pow10(scale))) * int64(math.Pow10(9-scale))
}

// Arrow Interface (Column) converter. This is called when Arrow chunks are
// downloaded to convert to the corresponding row type.
func arrowToValues(
	ctx context.Context,
	destcol []snowflakeValue,
	srcColumnMeta execResponseRowType,
	srcValue arrow.Array,
	loc *time.Location,
	higherPrecision bool,
	params map[string]*string) error {

	if len(destcol) != srcValue.Len() {
		return fmt.Errorf("array interface length mismatch")
	}
	logger.Debugf("snowflake data type: %v, arrow data type: %v", srcColumnMeta.Type, srcValue.DataType())

	var err error
	snowflakeType := getSnowflakeType(srcColumnMeta.Type)
	for i := range destcol {
		if destcol[i], err = arrowToValue(ctx, i, srcColumnMeta.toFieldMetadata(), srcValue, loc, higherPrecision, params, snowflakeType); err != nil {
			return err
		}
	}
	return nil
}

func arrowToValue(ctx context.Context, rowIdx int, srcColumnMeta fieldMetadata, srcValue arrow.Array, loc *time.Location, higherPrecision bool, params map[string]*string, snowflakeType snowflakeType) (snowflakeValue, error) {
	switch snowflakeType {
	case fixedType:
		// Snowflake data types that are fixed-point numbers will fall into this category
		// e.g. NUMBER, DECIMAL/NUMERIC, INT/INTEGER
		switch numericValue := srcValue.(type) {
		case *array.Decimal128:
			return arrowDecimal128ToValue(numericValue, rowIdx, higherPrecision, int(srcColumnMeta.Scale)), nil
		case *array.Int64:
			return arrowInt64ToValue(numericValue, rowIdx, higherPrecision, int(srcColumnMeta.Scale)), nil
		case *array.Int32:
			return arrowInt32ToValue(numericValue, rowIdx, higherPrecision, int(srcColumnMeta.Scale)), nil
		case *array.Int16:
			return arrowInt16ToValue(numericValue, rowIdx, higherPrecision, int(srcColumnMeta.Scale)), nil
		case *array.Int8:
			return arrowInt8ToValue(numericValue, rowIdx, higherPrecision, int(srcColumnMeta.Scale)), nil
		}
		return nil, fmt.Errorf("unsupported data type")
	case booleanType:
		return arrowBoolToValue(srcValue.(*array.Boolean), rowIdx), nil
	case realType:
		// Snowflake data types that are floating-point numbers will fall in this category
		// e.g. FLOAT/REAL/DOUBLE
		return arrowRealToValue(srcValue.(*array.Float64), rowIdx), nil
	case textType, variantType:
		strings := srcValue.(*array.String)
		if !srcValue.IsNull(rowIdx) {
			return strings.Value(rowIdx), nil
		}
		return nil, nil
	case arrayType:
		if len(srcColumnMeta.Fields) == 0 {
			// semistructured type without schema
			strings := srcValue.(*array.String)
			if !srcValue.IsNull(rowIdx) {
				return strings.Value(rowIdx), nil
			}
			return nil, nil
		}
		strings, ok := srcValue.(*array.String)
		if ok {
			// structured array as json
			if !srcValue.IsNull(rowIdx) {
				val := strings.Value(rowIdx)
				var arr []any
				decoder := decoderWithNumbersAsStrings(&val)
				if err := decoder.Decode(&arr); err != nil {
					return nil, err
				}
				return buildStructuredArray(ctx, srcColumnMeta.Fields[0], arr, params)
			}
			return nil, nil
		}
		return buildListFromNativeArrow(ctx, rowIdx, srcColumnMeta.Fields[0], srcValue, loc, higherPrecision, params)
	case objectType:
		if len(srcColumnMeta.Fields) == 0 {
			// semistructured type without schema
			strings := srcValue.(*array.String)
			if !srcValue.IsNull(rowIdx) {
				return strings.Value(rowIdx), nil
			}
			return nil, nil
		}
		strings, ok := srcValue.(*array.String)
		if ok {
			// structured objects as json
			if !srcValue.IsNull(rowIdx) {
				m := make(map[string]any)
				value := strings.Value(rowIdx)
				decoder := decoderWithNumbersAsStrings(&value)
				if err := decoder.Decode(&m); err != nil {
					return nil, err
				}
				return buildStructuredTypeRecursive(ctx, m, srcColumnMeta.Fields, params)
			}
			return nil, nil
		}
		// structured objects as native arrow
		structs := srcValue.(*array.Struct)
		return arrowToStructuredType(ctx, structs, srcColumnMeta.Fields, loc, rowIdx, higherPrecision, params)
	case mapType:
		if srcValue.IsNull(rowIdx) {
			return nil, nil
		}
		strings, ok := srcValue.(*array.String)
		if ok {
			// structured map as json
			if !srcValue.IsNull(rowIdx) {
				return jsonToMap(ctx, srcColumnMeta.Fields[0], srcColumnMeta.Fields[1], strings.Value(rowIdx), params)
			}
		} else {
			// structured map as native arrow
			return buildMapFromNativeArrow(ctx, rowIdx, srcColumnMeta.Fields[0], srcColumnMeta.Fields[1], srcValue, loc, higherPrecision, params)
		}
	case binaryType:
		return arrowBinaryToValue(srcValue.(*array.Binary), rowIdx), nil
	case dateType:
		return arrowDateToValue(srcValue.(*array.Date32), rowIdx), nil
	case timeType:
		return arrowTimeToValue(srcValue, rowIdx, int(srcColumnMeta.Scale)), nil
	case timestampNtzType, timestampLtzType, timestampTzType:
		v := arrowSnowflakeTimestampToTime(srcValue, snowflakeType, int(srcColumnMeta.Scale), rowIdx, loc)
		if v != nil {
			return *v, nil
		}
		return nil, nil
	}

	return nil, fmt.Errorf("unsupported data type")
}

func buildMapFromNativeArrow(ctx context.Context, rowIdx int, keyMetadata, valueMetadata fieldMetadata, srcValue arrow.Array, loc *time.Location, higherPrecision bool, params map[string]*string) (snowflakeValue, error) {
	arrowMap := srcValue.(*array.Map)
	if arrowMap.IsNull(rowIdx) {
		return nil, nil
	}
	keys := arrowMap.Keys()
	items := arrowMap.Items()
	offsets := arrowMap.Offsets()
	switch keyMetadata.Type {
	case "text":
		keyFunc := func(j int) (string, error) {
			return keys.(*array.String).Value(j), nil
		}
		return buildStructuredMapFromArrow(ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
	case "fixed":
		keyFunc := func(j int) (int64, error) {
			k, err := extractInt64(keys, int(j))
			if err != nil {
				return 0, err
			}
			return k, nil
		}
		return buildStructuredMapFromArrow(ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
	}
	return nil, nil
}

func buildListFromNativeArrow(ctx context.Context, rowIdx int, fieldMetadata fieldMetadata, srcValue arrow.Array, loc *time.Location, higherPrecision bool, params map[string]*string) (snowflakeValue, error) {
	list := srcValue.(*array.List)
	if list.IsNull(rowIdx) {
		return nil, nil
	}
	values := list.ListValues()
	offsets := list.Offsets()
	snowflakeType := getSnowflakeType(fieldMetadata.Type)
	switch snowflakeType {
	case fixedType:
		switch typedValues := values.(type) {
		case *array.Decimal128:
			if higherPrecision && fieldMetadata.Scale == 0 {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (*big.Int, error) {
					return arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale).(*big.Int), nil
				})
			} else if higherPrecision && fieldMetadata.Scale != 0 {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (*big.Float, error) {
					return arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale).(*big.Float), nil
				})
			} else if !higherPrecision && fieldMetadata.Scale == 0 {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int64, error) {
					v := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale)
					return strconv.ParseInt(v.(string), 10, 64)
				})
			} else {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (float64, error) {
					v := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale)
					return strconv.ParseFloat(v.(string), 64)
				})
			}
		case *array.Int64:
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int64, error) {
				return arrowInt64ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale).(int64), nil
			})
		case *array.Int32:
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int32, error) {
				return arrowInt32ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale).(int32), nil
			})
		case *array.Int16:
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int16, error) {
				return arrowInt16ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale).(int16), nil
			})
		case *array.Int8:
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int8, error) {
				return arrowInt8ToValue(typedValues, j, higherPrecision, fieldMetadata.Scale).(int8), nil
			})
		}
	case realType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (float64, error) {
			return arrowRealToValue(values.(*array.Float64), j).(float64), nil
		})
	case textType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (string, error) {
			return arrowStringToValue(values.(*array.String), j).(string), nil
		})
	case booleanType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (bool, error) {
			return arrowBoolToValue(values.(*array.Boolean), j).(bool), nil
		})
	case binaryType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) ([]byte, error) {
			return arrowBinaryToValue(values.(*array.Binary), j).([]byte), nil
		})
	case dateType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (time.Time, error) {
			return arrowDateToValue(values.(*array.Date32), int(j)).(time.Time), nil
		})
	case timeType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (time.Time, error) {
			return arrowTimeToValue(values, int(j), fieldMetadata.Scale).(time.Time), nil
		})
	case timestampNtzType, timestampLtzType, timestampTzType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (time.Time, error) {
			ptr := arrowSnowflakeTimestampToTime(values, snowflakeType, fieldMetadata.Scale, j, loc)
			if ptr != nil {
				return *ptr, nil
			}
			return time.Time{}, nil
		})
	case objectType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (*structuredType, error) {
			m := make(map[string]any, len(fieldMetadata.Fields))
			for fieldIdx, field := range fieldMetadata.Fields {
				m[field.Name] = values.(*array.Struct).Field(fieldIdx).ValueStr(int(j))
			}
			return buildStructuredTypeRecursive(ctx, m, fieldMetadata.Fields, params)
		})
	case arrayType:
		switch fieldMetadata.Fields[0].Type {
		case "text":
			return buildArrowListRecursive[string](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "fixed":
			if fieldMetadata.Fields[0].Scale == 0 {
				return buildArrowListRecursive[int64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			return buildArrowListRecursive[float64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "real":
			return buildArrowListRecursive[float64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "boolean":
			return buildArrowListRecursive[bool](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "binary":
			return buildArrowListRecursive[[]byte](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "date", "time", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
			return buildArrowListRecursive[time.Time](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		}
	}
	return nil, nil
}

func buildArrowListRecursive[T any](ctx context.Context, rowIdx int, fieldMetadata fieldMetadata, offsets []int32, values arrow.Array, loc *time.Location, higherPrecision bool, params map[string]*string) (snowflakeValue, error) {
	return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) ([]T, error) {
		arrowList, err := buildListFromNativeArrow(ctx, j, fieldMetadata.Fields[0], values, loc, higherPrecision, params)
		if err != nil {
			return nil, err
		}
		return arrowList.([]T), nil
	})
}

func mapStructuredArrayNativeArrowRows[T any](offsets []int32, rowIdx int, createValueFunc func(j int) (T, error)) (snowflakeValue, error) {
	arr := make([]T, offsets[rowIdx+1]-offsets[rowIdx])
	for j := offsets[rowIdx]; j < offsets[rowIdx+1]; j++ {
		v, err := createValueFunc(int(j))
		if err != nil {
			return nil, err
		}
		arr[j-offsets[rowIdx]] = v
	}
	return arr, nil
}

func extractInt64(values arrow.Array, j int) (int64, error) {
	switch typedValues := values.(type) {
	case *array.Decimal128:
		return int64(typedValues.Value(j).LowBits()), nil
	case *array.Int64:
		return typedValues.Value(j), nil
	case *array.Int32:
		return int64(typedValues.Value(j)), nil
	case *array.Int16:
		return int64(typedValues.Value(j)), nil
	case *array.Int8:
		return int64(typedValues.Value(j)), nil
	}
	return 0, fmt.Errorf("unsupported map type: %T", values.DataType().Name())
}

func buildStructuredMapFromArrow[K comparable](ctx context.Context, rowIdx int, valueMetadata fieldMetadata, offsets []int32, keyFunc func(j int) (K, error), items arrow.Array, higherPrecision bool, loc *time.Location, params map[string]*string) (snowflakeValue, error) {
	mapNullValuesEnabled := mapValuesNullableEnabled(ctx)
	switch valueMetadata.Type {
	case "text":
		if mapNullValuesEnabled {
			return mapStructuredMapNativeArrowRows(make(map[K]sql.NullString), offsets, rowIdx, keyFunc, func(j int) (sql.NullString, error) {
				if items.IsNull(j) {
					return sql.NullString{Valid: false}, nil
				}
				return sql.NullString{Valid: true, String: items.(*array.String).Value(j)}, nil
			})
		}
		return mapStructuredMapNativeArrowRows(make(map[K]string), offsets, rowIdx, keyFunc, func(j int) (string, error) {
			return items.(*array.String).Value(j), nil
		})
	case "boolean":
		if mapNullValuesEnabled {
			return mapStructuredMapNativeArrowRows(make(map[K]sql.NullBool), offsets, rowIdx, keyFunc, func(j int) (sql.NullBool, error) {
				if items.IsNull(j) {
					return sql.NullBool{Valid: false}, nil
				}
				return sql.NullBool{Valid: true, Bool: items.(*array.Boolean).Value(j)}, nil
			})
		}
		return mapStructuredMapNativeArrowRows(make(map[K]bool), offsets, rowIdx, keyFunc, func(j int) (bool, error) {
			return items.(*array.Boolean).Value(j), nil
		})
	case "fixed":
		if higherPrecision && valueMetadata.Scale == 0 {
			return mapStructuredMapNativeArrowRows(make(map[K]*big.Int), offsets, rowIdx, keyFunc, func(j int) (*big.Int, error) {
				if items.IsNull(j) {
					return nil, nil
				}
				return mapStructuredMapNativeArrowFixedValue[*big.Int](valueMetadata, j, items, higherPrecision, nil)
			})
		} else if higherPrecision && valueMetadata.Scale != 0 {
			return mapStructuredMapNativeArrowRows(make(map[K]*big.Float), offsets, rowIdx, keyFunc, func(j int) (*big.Float, error) {
				if items.IsNull(j) {
					return nil, nil
				}
				return mapStructuredMapNativeArrowFixedValue[*big.Float](valueMetadata, j, items, higherPrecision, nil)
			})
		} else if !higherPrecision && valueMetadata.Scale == 0 {
			if mapNullValuesEnabled {
				return mapStructuredMapNativeArrowRows(make(map[K]sql.NullInt64), offsets, rowIdx, keyFunc, func(j int) (sql.NullInt64, error) {
					if items.IsNull(j) {
						return sql.NullInt64{Valid: false}, nil
					}
					s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
					if err != nil {
						return sql.NullInt64{}, err
					}
					i64, err := strconv.ParseInt(s, 10, 64)
					return sql.NullInt64{Valid: true, Int64: i64}, err
				})
			}
			return mapStructuredMapNativeArrowRows(make(map[K]int64), offsets, rowIdx, keyFunc, func(j int) (int64, error) {
				s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
				if err != nil {
					return 0, err
				}
				return strconv.ParseInt(s, 10, 64)
			})
		} else {
			if mapNullValuesEnabled {
				return mapStructuredMapNativeArrowRows(make(map[K]sql.NullFloat64), offsets, rowIdx, keyFunc, func(j int) (sql.NullFloat64, error) {
					if items.IsNull(j) {
						return sql.NullFloat64{Valid: false}, nil
					}
					s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
					if err != nil {
						return sql.NullFloat64{}, err
					}
					f64, err := strconv.ParseFloat(s, 64)
					return sql.NullFloat64{Valid: true, Float64: f64}, err
				})
			}
			return mapStructuredMapNativeArrowRows(make(map[K]float64), offsets, rowIdx, keyFunc, func(j int) (float64, error) {
				s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
				if err != nil {
					return 0, err
				}
				return strconv.ParseFloat(s, 64)
			})
		}
	case "real":
		if mapNullValuesEnabled {
			return mapStructuredMapNativeArrowRows(make(map[K]sql.NullFloat64), offsets, rowIdx, keyFunc, func(j int) (sql.NullFloat64, error) {
				if items.IsNull(j) {
					return sql.NullFloat64{Valid: false}, nil
				}
				f64 := items.(*array.Float64).Value(j)
				return sql.NullFloat64{Valid: true, Float64: f64}, nil
			})
		}
		return mapStructuredMapNativeArrowRows(make(map[K]float64), offsets, rowIdx, keyFunc, func(j int) (float64, error) {
			return arrowRealToValue(items.(*array.Float64), j).(float64), nil
		})
	case "binary":
		return mapStructuredMapNativeArrowRows(make(map[K][]byte), offsets, rowIdx, keyFunc, func(j int) ([]byte, error) {
			if items.IsNull(j) {
				return nil, nil
			}
			return arrowBinaryToValue(items.(*array.Binary), j).([]byte), nil
		})
	case "date":
		return buildTimeFromNativeArrowArray(mapNullValuesEnabled, offsets, rowIdx, keyFunc, items, func(j int) time.Time {
			return arrowDateToValue(items.(*array.Date32), j).(time.Time)
		})
	case "time":
		return buildTimeFromNativeArrowArray(mapNullValuesEnabled, offsets, rowIdx, keyFunc, items, func(j int) time.Time {
			return arrowTimeToValue(items, j, valueMetadata.Scale).(time.Time)
		})
	case "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
		return buildTimeFromNativeArrowArray(mapNullValuesEnabled, offsets, rowIdx, keyFunc, items, func(j int) time.Time {
			return *arrowSnowflakeTimestampToTime(items, getSnowflakeType(valueMetadata.Type), valueMetadata.Scale, j, loc)
		})
	case "object":
		return mapStructuredMapNativeArrowRows(make(map[K]*structuredType), offsets, rowIdx, keyFunc, func(j int) (*structuredType, error) {
			if items.IsNull(j) {
				return nil, nil
			}
			var err error
			m := make(map[string]any)
			for fieldIdx, field := range valueMetadata.Fields {
				snowflakeType := getSnowflakeType(field.Type)
				m[field.Name], err = arrowToValue(ctx, j, field, items.(*array.Struct).Field(fieldIdx), loc, higherPrecision, params, snowflakeType)
				if err != nil {
					return nil, err
				}
			}
			return &structuredType{
				values:        m,
				fieldMetadata: valueMetadata.Fields,
				params:        params,
			}, nil
		})
	case "array":
		switch valueMetadata.Fields[0].Type {
		case "text":
			return buildListFromNativeArrowMap[K, string](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "fixed":
			if valueMetadata.Fields[0].Scale == 0 {
				return buildListFromNativeArrowMap[K, int64](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
			}
			return buildListFromNativeArrowMap[K, float64](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "real":
			return buildListFromNativeArrowMap[K, float64](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "binary":
			return buildListFromNativeArrowMap[K, []byte](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "boolean":
			return buildListFromNativeArrowMap[K, bool](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "date", "time", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
			return buildListFromNativeArrowMap[K, time.Time](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		}
	}
	return nil, errors.New("Unsupported map value: " + valueMetadata.Type)
}

func buildListFromNativeArrowMap[K comparable, V any](ctx context.Context, rowIdx int, valueMetadata fieldMetadata, offsets []int32, keyFunc func(j int) (K, error), items arrow.Array, higherPrecision bool, loc *time.Location, params map[string]*string) (snowflakeValue, error) {
	return mapStructuredMapNativeArrowRows(make(map[K][]V), offsets, rowIdx, keyFunc, func(j int) ([]V, error) {
		if items.IsNull(j) {
			return []V{}, nil
		}
		list, err := buildListFromNativeArrow(ctx, j, valueMetadata.Fields[0], items, loc, higherPrecision, params)
		return list.([]V), err
	})
}

func buildTimeFromNativeArrowArray[K comparable](mapNullValuesEnabled bool, offsets []int32, rowIdx int, keyFunc func(j int) (K, error), items arrow.Array, buildTime func(j int) time.Time) (snowflakeValue, error) {
	if mapNullValuesEnabled {
		return mapStructuredMapNativeArrowRows(make(map[K]sql.NullTime), offsets, rowIdx, keyFunc, func(j int) (sql.NullTime, error) {
			if items.IsNull(j) {
				return sql.NullTime{Valid: false}, nil
			}
			return sql.NullTime{Valid: true, Time: buildTime(j)}, nil
		})
	}
	return mapStructuredMapNativeArrowRows(make(map[K]time.Time), offsets, rowIdx, keyFunc, func(j int) (time.Time, error) {
		return buildTime(j), nil
	})
}

func mapStructuredMapNativeArrowFixedValue[V any](valueMetadata fieldMetadata, j int, items arrow.Array, higherPrecision bool, defaultValue V) (V, error) {
	v, err := extractNumberFromArrow(&items, j, higherPrecision, valueMetadata.Scale)
	if err != nil {
		return defaultValue, err
	}
	return v.(V), nil
}

func extractNumberFromArrow(values *arrow.Array, j int, higherPrecision bool, scale int) (snowflakeValue, error) {
	switch typedValues := (*values).(type) {
	case *array.Decimal128:
		return arrowDecimal128ToValue(typedValues, j, higherPrecision, scale), nil
	case *array.Int64:
		return arrowInt64ToValue(typedValues, j, higherPrecision, scale), nil
	case *array.Int32:
		return arrowInt32ToValue(typedValues, j, higherPrecision, scale), nil
	case *array.Int16:
		return arrowInt16ToValue(typedValues, j, higherPrecision, scale), nil
	case *array.Int8:
		return arrowInt8ToValue(typedValues, j, higherPrecision, scale), nil
	}
	return 0, fmt.Errorf("unknown number type: %T", values)
}

func mapStructuredMapNativeArrowRows[K comparable, V any](m map[K]V, offsets []int32, rowIdx int, keyFunc func(j int) (K, error), itemFunc func(j int) (V, error)) (map[K]V, error) {
	for j := offsets[rowIdx]; j < offsets[rowIdx+1]; j++ {
		k, err := keyFunc(int(j))
		if err != nil {
			return nil, err
		}
		if m[k], err = itemFunc(int(j)); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func arrowToStructuredType(ctx context.Context, structs *array.Struct, fieldMetadata []fieldMetadata, loc *time.Location, rowIdx int, higherPrecision bool, params map[string]*string) (*structuredType, error) {
	var err error
	m := make(map[string]any)
	for colIdx := 0; colIdx < structs.NumField(); colIdx++ {
		var v any
		switch getSnowflakeType(fieldMetadata[colIdx].Type) {
		case fixedType:
			v = structs.Field(colIdx).ValueStr(rowIdx)
			switch typedValues := structs.Field(colIdx).(type) {
			case *array.Decimal128:
				v = arrowDecimal128ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx].Scale)
			case *array.Int64:
				v = arrowInt64ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx].Scale)
			case *array.Int32:
				v = arrowInt32ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx].Scale)
			case *array.Int16:
				v = arrowInt16ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx].Scale)
			case *array.Int8:
				v = arrowInt8ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx].Scale)
			}
		case booleanType:
			v = arrowBoolToValue(structs.Field(colIdx).(*array.Boolean), rowIdx)
		case realType:
			v = arrowRealToValue(structs.Field(colIdx).(*array.Float64), rowIdx)
		case binaryType:
			v = arrowBinaryToValue(structs.Field(colIdx).(*array.Binary), rowIdx)
		case dateType:
			v = arrowDateToValue(structs.Field(colIdx).(*array.Date32), rowIdx)
		case timeType:
			v = arrowTimeToValue(structs.Field(colIdx), rowIdx, fieldMetadata[colIdx].Scale)
		case textType:
			v = arrowStringToValue(structs.Field(colIdx).(*array.String), rowIdx)
		case timestampLtzType, timestampTzType, timestampNtzType:
			ptr := arrowSnowflakeTimestampToTime(structs.Field(colIdx), getSnowflakeType(fieldMetadata[colIdx].Type), fieldMetadata[colIdx].Scale, rowIdx, loc)
			if ptr != nil {
				v = *ptr
			}
		case objectType:
			if !structs.Field(colIdx).IsNull(rowIdx) {
				if v, err = arrowToStructuredType(ctx, structs.Field(colIdx).(*array.Struct), fieldMetadata[colIdx].Fields, loc, rowIdx, higherPrecision, params); err != nil {
					return nil, err
				}
			}
		case arrayType:
			if !structs.Field(colIdx).IsNull(rowIdx) {
				var err error
				if v, err = buildListFromNativeArrow(ctx, rowIdx, fieldMetadata[colIdx].Fields[0], structs.Field(colIdx), loc, higherPrecision, params); err != nil {
					return nil, err
				}
			}
		case mapType:
			if !structs.Field(colIdx).IsNull(rowIdx) {
				var err error
				if v, err = buildMapFromNativeArrow(ctx, rowIdx, fieldMetadata[colIdx].Fields[0], fieldMetadata[colIdx].Fields[1], structs.Field(colIdx), loc, higherPrecision, params); err != nil {
					return nil, err
				}
			}
		}
		m[fieldMetadata[colIdx].Name] = v
	}
	return &structuredType{
		values:        m,
		fieldMetadata: fieldMetadata,
		params:        params,
	}, nil
}

func arrowStringToValue(srcValue *array.String, rowIdx int) snowflakeValue {
	if srcValue.IsNull(rowIdx) {
		return nil
	}
	return srcValue.Value(rowIdx)
}

func arrowDecimal128ToValue(srcValue *array.Decimal128, rowIdx int, higherPrecision bool, scale int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		num := srcValue.Value(rowIdx)
		if scale == 0 {
			if higherPrecision {
				return num.BigInt()
			}
			return num.ToString(0)
		}
		f := decimalToBigFloat(num, int64(scale))
		if higherPrecision {
			return f
		}
		return fmt.Sprintf("%.*f", scale, f)
	}
	return nil
}

func arrowInt64ToValue(srcValue *array.Int64, rowIdx int, higherPrecision bool, scale int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(scale, higherPrecision, val)
	}
	return nil
}

func arrowInt32ToValue(srcValue *array.Int32, rowIdx int, higherPrecision bool, scale int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(scale, higherPrecision, int64(val))
	}
	return nil
}

func arrowInt16ToValue(srcValue *array.Int16, rowIdx int, higherPrecision bool, scale int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(scale, higherPrecision, int64(val))
	}
	return nil
}

func arrowInt8ToValue(srcValue *array.Int8, rowIdx int, higherPrecision bool, scale int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(scale, higherPrecision, int64(val))
	}
	return nil
}

func arrowIntToValue(scale int, higherPrecision bool, val int64) snowflakeValue {
	if scale == 0 {
		if higherPrecision {
			return int64(val)
		}
		return fmt.Sprintf("%d", val)
	}
	if higherPrecision {
		f := intToBigFloat(int64(val), int64(scale))
		return f
	}
	return fmt.Sprintf("%.*f", scale, float64(val)/math.Pow10(int(scale)))
}

func arrowRealToValue(srcValue *array.Float64, rowIdx int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		return srcValue.Value(rowIdx)
	}
	return nil
}

func arrowBoolToValue(srcValue *array.Boolean, rowIdx int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		return srcValue.Value(rowIdx)
	}
	return nil
}

func arrowBinaryToValue(srcValue *array.Binary, rowIdx int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		return srcValue.Value(rowIdx)
	}
	return nil
}

func arrowDateToValue(srcValue *array.Date32, rowID int) snowflakeValue {
	if !srcValue.IsNull(rowID) {
		return time.Unix(int64(srcValue.Value(rowID))*86400, 0).UTC()
	}
	return nil
}

func arrowTimeToValue(srcValue arrow.Array, rowIdx int, scale int) snowflakeValue {
	t0 := time.Time{}
	if srcValue.DataType().ID() == arrow.INT64 {
		if !srcValue.IsNull(rowIdx) {
			return t0.Add(time.Duration(srcValue.(*array.Int64).Value(rowIdx) * int64(math.Pow10(9-scale))))
		}
	} else {
		if !srcValue.IsNull(rowIdx) {
			return t0.Add(time.Duration(int64(srcValue.(*array.Int32).Value(rowIdx)) * int64(math.Pow10(9-scale))))
		}
	}
	return nil
}

type (
	intArray          []int
	int32Array        []int32
	int64Array        []int64
	float64Array      []float64
	float32Array      []float32
	boolArray         []bool
	stringArray       []string
	byteArray         [][]byte
	timestampNtzArray []time.Time
	timestampLtzArray []time.Time
	timestampTzArray  []time.Time
	dateArray         []time.Time
	timeArray         []time.Time
)

// Array takes in a column of a row to be inserted via array binding, bulk or
// otherwise, and converts it into a native snowflake type for binding
func Array(a interface{}, typ ...timezoneType) interface{} {
	switch t := a.(type) {
	case []int:
		return (*intArray)(&t)
	case []int32:
		return (*int32Array)(&t)
	case []int64:
		return (*int64Array)(&t)
	case []float64:
		return (*float64Array)(&t)
	case []float32:
		return (*float32Array)(&t)
	case []bool:
		return (*boolArray)(&t)
	case []string:
		return (*stringArray)(&t)
	case [][]byte:
		return (*byteArray)(&t)
	case []time.Time:
		if len(typ) < 1 {
			return a
		}
		switch typ[0] {
		case TimestampNTZType:
			return (*timestampNtzArray)(&t)
		case TimestampLTZType:
			return (*timestampLtzArray)(&t)
		case TimestampTZType:
			return (*timestampTzArray)(&t)
		case DateType:
			return (*dateArray)(&t)
		case TimeType:
			return (*timeArray)(&t)
		default:
			return a
		}
	case *[]int:
		return (*intArray)(t)
	case *[]int32:
		return (*int32Array)(t)
	case *[]int64:
		return (*int64Array)(t)
	case *[]float64:
		return (*float64Array)(t)
	case *[]float32:
		return (*float32Array)(t)
	case *[]bool:
		return (*boolArray)(t)
	case *[]string:
		return (*stringArray)(t)
	case *[][]byte:
		return (*byteArray)(t)
	case *[]time.Time:
		if len(typ) < 1 {
			return a
		}
		switch typ[0] {
		case TimestampNTZType:
			return (*timestampNtzArray)(t)
		case TimestampLTZType:
			return (*timestampLtzArray)(t)
		case TimestampTZType:
			return (*timestampTzArray)(t)
		case DateType:
			return (*dateArray)(t)
		case TimeType:
			return (*timeArray)(t)
		default:
			return a
		}
	case []interface{}, *[]interface{}:
		// Support for bulk array binding insertion using []interface{}
		if len(typ) < 1 {
			return interfaceArrayBinding{
				hasTimezone:       false,
				timezoneTypeArray: a,
			}
		}
		return interfaceArrayBinding{
			hasTimezone:       true,
			tzType:            typ[0],
			timezoneTypeArray: a,
		}
	default:
		return a
	}
}

// snowflakeArrayToString converts the array binding to snowflake's native
// string type. The string value differs whether it's directly bound or
// uploaded via stream.
func snowflakeArrayToString(nv *driver.NamedValue, stream bool) (snowflakeType, []*string) {
	var t snowflakeType
	var arr []*string
	switch reflect.TypeOf(nv.Value) {
	case reflect.TypeOf(&intArray{}):
		t = fixedType
		a := nv.Value.(*intArray)
		for _, x := range *a {
			v := strconv.Itoa(x)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&int64Array{}):
		t = fixedType
		a := nv.Value.(*int64Array)
		for _, x := range *a {
			v := strconv.FormatInt(x, 10)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&int32Array{}):
		t = fixedType
		a := nv.Value.(*int32Array)
		for _, x := range *a {
			v := strconv.Itoa(int(x))
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&float64Array{}):
		t = realType
		a := nv.Value.(*float64Array)
		for _, x := range *a {
			v := fmt.Sprintf("%g", x)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&float32Array{}):
		t = realType
		a := nv.Value.(*float32Array)
		for _, x := range *a {
			v := fmt.Sprintf("%g", x)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&boolArray{}):
		t = booleanType
		a := nv.Value.(*boolArray)
		for _, x := range *a {
			v := strconv.FormatBool(x)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&stringArray{}):
		t = textType
		a := nv.Value.(*stringArray)
		for _, x := range *a {
			v := x // necessary for address to be not overwritten
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&byteArray{}):
		t = binaryType
		a := nv.Value.(*byteArray)
		for _, x := range *a {
			v := hex.EncodeToString(x)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&timestampNtzArray{}):
		t = timestampNtzType
		a := nv.Value.(*timestampNtzArray)
		for _, x := range *a {
			v := strconv.FormatInt(x.UnixNano(), 10)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&timestampLtzArray{}):
		t = timestampLtzType
		a := nv.Value.(*timestampLtzArray)
		for _, x := range *a {
			v := strconv.FormatInt(x.UnixNano(), 10)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&timestampTzArray{}):
		t = timestampTzType
		a := nv.Value.(*timestampTzArray)
		for _, x := range *a {
			var v string
			if stream {
				v = x.Format(format)
			} else {
				_, offset := x.Zone()
				v = fmt.Sprintf("%v %v", x.UnixNano(), offset/60+1440)
			}
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&dateArray{}):
		t = dateType
		a := nv.Value.(*dateArray)
		for _, x := range *a {
			_, offset := x.Zone()
			x = x.Add(time.Second * time.Duration(offset))
			v := fmt.Sprintf("%d", x.Unix()*1000)
			arr = append(arr, &v)
		}
	case reflect.TypeOf(&timeArray{}):
		t = timeType
		a := nv.Value.(*timeArray)
		for _, x := range *a {
			var v string
			if stream {
				v = fmt.Sprintf("%02d:%02d:%02d.%09d", x.Hour(), x.Minute(), x.Second(), x.Nanosecond())
			} else {
				h, m, s := x.Clock()
				tm := int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) + int64(s)*int64(time.Second) + int64(x.Nanosecond())
				v = strconv.FormatInt(tm, 10)
			}
			arr = append(arr, &v)
		}
	default:
		// Support for bulk array binding insertion using []interface{}
		nvValue := reflect.ValueOf(nv)
		if nvValue.Kind() == reflect.Ptr {
			value := reflect.Indirect(reflect.ValueOf(nv.Value))
			if isInterfaceArrayBinding(value.Interface()) {
				timeStruct, ok := value.Interface().(interfaceArrayBinding)
				if ok {
					timeInterfaceSlice := reflect.Indirect(reflect.ValueOf(timeStruct.timezoneTypeArray))
					if timeStruct.hasTimezone {
						return interfaceSliceToString(timeInterfaceSlice, stream, timeStruct.tzType)
					}
					return interfaceSliceToString(timeInterfaceSlice, stream)
				}
			}
		}
		return unSupportedType, nil
	}
	return t, arr
}

func interfaceSliceToString(interfaceSlice reflect.Value, stream bool, tzType ...timezoneType) (snowflakeType, []*string) {
	var t snowflakeType
	var arr []*string

	for i := 0; i < interfaceSlice.Len(); i++ {
		val := interfaceSlice.Index(i)
		if val.CanInterface() {
			switch val.Interface().(type) {
			case int:
				t = fixedType
				x := val.Interface().(int)
				v := strconv.Itoa(x)
				arr = append(arr, &v)
			case int32:
				t = fixedType
				x := val.Interface().(int32)
				v := strconv.Itoa(int(x))
				arr = append(arr, &v)
			case int64:
				t = fixedType
				x := val.Interface().(int64)
				v := strconv.FormatInt(x, 10)
				arr = append(arr, &v)
			case float32:
				t = realType
				x := val.Interface().(float32)
				v := fmt.Sprintf("%g", x)
				arr = append(arr, &v)
			case float64:
				t = realType
				x := val.Interface().(float64)
				v := fmt.Sprintf("%g", x)
				arr = append(arr, &v)
			case bool:
				t = booleanType
				x := val.Interface().(bool)
				v := strconv.FormatBool(x)
				arr = append(arr, &v)
			case string:
				t = textType
				x := val.Interface().(string)
				arr = append(arr, &x)
			case []byte:
				t = binaryType
				x := val.Interface().([]byte)
				v := hex.EncodeToString(x)
				arr = append(arr, &v)
			case time.Time:
				if len(tzType) < 1 {
					return unSupportedType, nil
				}

				x := val.Interface().(time.Time)
				switch tzType[0] {
				case TimestampNTZType:
					t = timestampNtzType
					v := strconv.FormatInt(x.UnixNano(), 10)
					arr = append(arr, &v)
				case TimestampLTZType:
					t = timestampLtzType
					v := strconv.FormatInt(x.UnixNano(), 10)
					arr = append(arr, &v)
				case TimestampTZType:
					t = timestampTzType
					var v string
					if stream {
						v = x.Format(format)
					} else {
						_, offset := x.Zone()
						v = fmt.Sprintf("%v %v", x.UnixNano(), offset/60+1440)
					}
					arr = append(arr, &v)
				case DateType:
					t = dateType
					_, offset := x.Zone()
					x = x.Add(time.Second * time.Duration(offset))
					v := fmt.Sprintf("%d", x.Unix()*1000)
					arr = append(arr, &v)
				case TimeType:
					t = timeType
					var v string
					if stream {
						v = x.Format(format[11:19])
					} else {
						h, m, s := x.Clock()
						tm := int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) + int64(s)*int64(time.Second) + int64(x.Nanosecond())
						v = strconv.FormatInt(tm, 10)
					}
					arr = append(arr, &v)
				default:
					return unSupportedType, nil
				}
			default:
				if val.Interface() != nil {
					return unSupportedType, nil
				}

				arr = append(arr, nil)
			}
		}
	}
	return t, arr
}

func higherPrecisionEnabled(ctx context.Context) bool {
	v := ctx.Value(enableHigherPrecision)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

func arrowBatchesUtf8ValidationEnabled(ctx context.Context) bool {
	v := ctx.Value(enableArrowBatchesUtf8Validation)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

func getArrowBatchesTimestampOption(ctx context.Context) snowflakeArrowBatchesTimestampOption {
	v := ctx.Value(arrowBatchesTimestampOption)
	if v == nil {
		return UseNanosecondTimestamp
	}
	o, ok := v.(snowflakeArrowBatchesTimestampOption)
	if !ok {
		return UseNanosecondTimestamp
	}
	return o
}

func arrowToRecord(ctx context.Context, record arrow.Record, pool memory.Allocator, rowType []execResponseRowType, loc *time.Location) (arrow.Record, error) {
	arrowBatchesTimestampOption := getArrowBatchesTimestampOption(ctx)
	higherPrecisionEnabled := higherPrecisionEnabled(ctx)

	s, err := recordToSchema(record.Schema(), rowType, loc, arrowBatchesTimestampOption, higherPrecisionEnabled)
	if err != nil {
		return nil, err
	}

	var cols []arrow.Array
	numRows := record.NumRows()
	ctxAlloc := compute.WithAllocator(ctx, pool)

	for i, col := range record.Columns() {
		srcColumnMeta := rowType[i]

		// TODO: confirm that it is okay to be using higher precision logic for conversions
		newCol := col
		snowflakeType := getSnowflakeType(srcColumnMeta.Type)
		switch snowflakeType {
		case fixedType:
			var toType arrow.DataType
			if higherPrecisionEnabled {
				// do nothing - return decimal as is
			} else if col.DataType().ID() == arrow.DECIMAL || col.DataType().ID() == arrow.DECIMAL256 {
				if srcColumnMeta.Scale == 0 {
					toType = arrow.PrimitiveTypes.Int64
				} else {
					toType = arrow.PrimitiveTypes.Float64
				}
				// we're fine truncating so no error for data loss here.
				// so we use UnsafeCastOptions.
				newCol, err = compute.CastArray(ctxAlloc, col, compute.UnsafeCastOptions(toType))
				if err != nil {
					return nil, err
				}
				defer newCol.Release()
			} else if srcColumnMeta.Scale != 0 && col.DataType().ID() != arrow.INT64 {
				result, err := compute.Divide(ctxAlloc, compute.ArithmeticOptions{NoCheckOverflow: true},
					&compute.ArrayDatum{Value: newCol.Data()},
					compute.NewDatum(math.Pow10(int(srcColumnMeta.Scale))))
				if err != nil {
					return nil, err
				}
				defer result.Release()
				newCol = result.(*compute.ArrayDatum).MakeArray()
				defer newCol.Release()
			} else if srcColumnMeta.Scale != 0 && col.DataType().ID() == arrow.INT64 {
				// gosnowflake driver uses compute.Divide() which could bring `integer value not in range: -9007199254740992 to 9007199254740992` error
				// if we convert int64 to BigDecimal and then use compute.CastArray to convert BigDecimal to float64, we won't have enough precision.
				// e.g 0.1 as (38,19) will result 0.09999999999999999
				values := col.(*array.Int64).Int64Values()
				floatValues := make([]float64, len(values))
				for i, val := range values {
					floatValues[i], _ = intToBigFloat(val, srcColumnMeta.Scale).Float64()
				}
				builder := array.NewFloat64Builder(memory.NewCheckedAllocator(memory.NewGoAllocator()))
				builder.AppendValues(floatValues, nil)
				newCol = builder.NewArray()
				builder.Release()
				defer newCol.Release()
			}
		case timeType:
			newCol, err = compute.CastArray(ctxAlloc, col, compute.SafeCastOptions(arrow.FixedWidthTypes.Time64ns))
			if err != nil {
				return nil, err
			}
			defer newCol.Release()
		case timestampNtzType, timestampLtzType, timestampTzType:
			if arrowBatchesTimestampOption == UseOriginalTimestamp {
				// do nothing - return timestamp as is
			} else {
				var unit arrow.TimeUnit
				switch arrowBatchesTimestampOption {
				case UseMicrosecondTimestamp:
					unit = arrow.Microsecond
				case UseMillisecondTimestamp:
					unit = arrow.Millisecond
				case UseSecondTimestamp:
					unit = arrow.Second
				case UseNanosecondTimestamp:
					unit = arrow.Nanosecond
				}
				var tb *array.TimestampBuilder
				if snowflakeType == timestampLtzType {
					tb = array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: unit, TimeZone: loc.String()})
				} else {
					tb = array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: unit})
				}
				defer tb.Release()

				for i := 0; i < int(numRows); i++ {
					ts := arrowSnowflakeTimestampToTime(col, snowflakeType, int(srcColumnMeta.Scale), i, loc)
					if ts != nil {
						var ar arrow.Timestamp
						switch arrowBatchesTimestampOption {
						case UseMicrosecondTimestamp:
							ar = arrow.Timestamp(ts.UnixMicro())
						case UseMillisecondTimestamp:
							ar = arrow.Timestamp(ts.UnixMilli())
						case UseSecondTimestamp:
							ar = arrow.Timestamp(ts.Unix())
						case UseNanosecondTimestamp:
							ar = arrow.Timestamp(ts.UnixNano())
							// in case of overflow in arrow timestamp return error
							// this could only happen for nanosecond case
							if ts.UTC().Year() != ar.ToTime(arrow.Nanosecond).Year() {
								return nil, &SnowflakeError{
									Number:   ErrTooHighTimestampPrecision,
									SQLState: SQLStateInvalidDataTimeFormat,
									Message:  fmt.Sprintf("Cannot convert timestamp %v in column %v to Arrow.Timestamp data type due to too high precision. Please use context with WithOriginalTimestamp.", ts.UTC(), srcColumnMeta.Name),
								}
							}
						}
						tb.Append(ar)
					} else {
						tb.AppendNull()
					}
				}

				newCol = tb.NewArray()
				defer newCol.Release()
			}
		case textType:
			if arrowBatchesUtf8ValidationEnabled(ctx) && col.DataType().ID() == arrow.STRING {
				tb := array.NewStringBuilder(pool)
				defer tb.Release()

				for i := 0; i < int(numRows); i++ {
					if col.(*array.String).IsValid(i) {
						stringValue := col.(*array.String).Value(i)
						if !utf8.ValidString(stringValue) {
							logger.WithContext(ctx).Error("Invalid UTF-8 characters detected while reading query response, column: ", srcColumnMeta.Name)
							stringValue = strings.ToValidUTF8(stringValue, "�")
						}
						tb.Append(stringValue)
					} else {
						tb.AppendNull()
					}
				}
				newCol = tb.NewArray()
				defer newCol.Release()
			}
		}
		cols = append(cols, newCol)
	}
	return array.NewRecord(s, cols, numRows), nil
}

func recordToSchema(sc *arrow.Schema, rowType []execResponseRowType, loc *time.Location, timestampOption snowflakeArrowBatchesTimestampOption, withHigherPrecision bool) (*arrow.Schema, error) {
	var fields []arrow.Field
	for i := 0; i < len(sc.Fields()); i++ {
		f := sc.Field(i)
		srcColumnMeta := rowType[i]
		converted := true

		var t arrow.DataType
		switch getSnowflakeType(srcColumnMeta.Type) {
		case fixedType:
			switch f.Type.ID() {
			case arrow.DECIMAL:
				if withHigherPrecision {
					converted = false
				} else if srcColumnMeta.Scale == 0 {
					t = &arrow.Int64Type{}
				} else {
					t = &arrow.Float64Type{}
				}
			default:
				if withHigherPrecision {
					converted = false
				} else if srcColumnMeta.Scale != 0 {
					t = &arrow.Float64Type{}
				} else {
					converted = false
				}
			}
		case timeType:
			t = &arrow.Time64Type{Unit: arrow.Nanosecond}
		case timestampNtzType, timestampTzType:
			if timestampOption == UseOriginalTimestamp {
				// do nothing - return timestamp as is
				converted = false
			} else if timestampOption == UseMicrosecondTimestamp {
				t = &arrow.TimestampType{Unit: arrow.Microsecond}
			} else if timestampOption == UseMillisecondTimestamp {
				t = &arrow.TimestampType{Unit: arrow.Millisecond}
			} else if timestampOption == UseSecondTimestamp {
				t = &arrow.TimestampType{Unit: arrow.Second}
			} else {
				t = &arrow.TimestampType{Unit: arrow.Nanosecond}
			}
		case timestampLtzType:
			if timestampOption == UseOriginalTimestamp {
				// do nothing - return timestamp as is
				converted = false
			} else if timestampOption == UseMicrosecondTimestamp {
				t = &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: loc.String()}
			} else if timestampOption == UseMillisecondTimestamp {
				t = &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: loc.String()}
			} else if timestampOption == UseSecondTimestamp {
				t = &arrow.TimestampType{Unit: arrow.Second, TimeZone: loc.String()}
			} else {
				t = &arrow.TimestampType{Unit: arrow.Nanosecond, TimeZone: loc.String()}
			}
		default:
			converted = false
		}

		newField := f
		if converted {
			newField = arrow.Field{
				Name:     f.Name,
				Type:     t,
				Nullable: f.Nullable,
				Metadata: f.Metadata,
			}
		}
		fields = append(fields, newField)
	}
	meta := sc.Metadata()
	return arrow.NewSchema(fields, &meta), nil
}

// TypedNullTime is required to properly bind the null value with the snowflakeType as the Snowflake functions
// require the type of the field to be provided explicitly for the null values
type TypedNullTime struct {
	Time   sql.NullTime
	TzType timezoneType
}

func convertTzTypeToSnowflakeType(tzType timezoneType) snowflakeType {
	switch tzType {
	case TimestampNTZType:
		return timestampNtzType
	case TimestampLTZType:
		return timestampLtzType
	case TimestampTZType:
		return timestampTzType
	case DateType:
		return dateType
	case TimeType:
		return timeType
	}
	return unSupportedType
}

func decoderWithNumbersAsStrings(srcValue *string) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewBufferString(*srcValue))
	decoder.UseNumber()
	return decoder
}
