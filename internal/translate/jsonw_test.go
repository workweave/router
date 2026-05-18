package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONWriter_SimpleObject(t *testing.T) {
	w := newJSONWriter()
	w.Obj()
	w.Key("a")
	w.Str("b")
	w.Key("c")
	w.Int(1)
	w.EndObj()
	require.JSONEq(t, `{"a":"b","c":1}`, string(w.Bytes()))
}

func TestJSONWriter_NestedObject(t *testing.T) {
	w := newJSONWriter()
	w.Obj()
	w.Key("outer")
	w.Obj()
	w.Key("inner")
	w.Bool(true)
	w.EndObj()
	w.EndObj()
	require.JSONEq(t, `{"outer":{"inner":true}}`, string(w.Bytes()))
}

func TestJSONWriter_Array(t *testing.T) {
	w := newJSONWriter()
	w.Arr()
	w.Int(1)
	w.Int(2)
	w.Int(3)
	w.EndArr()
	require.JSONEq(t, `[1,2,3]`, string(w.Bytes()))
}

func TestJSONWriter_ArrayOfObjects(t *testing.T) {
	w := newJSONWriter()
	w.Arr()
	w.Obj()
	w.Key("x")
	w.Int(1)
	w.EndObj()
	w.Obj()
	w.Key("x")
	w.Int(2)
	w.EndObj()
	w.EndArr()
	require.JSONEq(t, `[{"x":1},{"x":2}]`, string(w.Bytes()))
}

func TestJSONWriter_Raw(t *testing.T) {
	w := newJSONWriter()
	w.Obj()
	w.Key("data")
	w.Raw(`{"nested":"value"}`)
	w.EndObj()
	require.JSONEq(t, `{"data":{"nested":"value"}}`, string(w.Bytes()))
}

func TestJSONWriter_Null(t *testing.T) {
	w := newJSONWriter()
	w.Obj()
	w.Key("key")
	w.Null()
	w.EndObj()
	require.JSONEq(t, `{"key":null}`, string(w.Bytes()))
}

func TestJSONWriter_EmptyContainers(t *testing.T) {
	w := newJSONWriter()
	w.Obj()
	w.Key("arr")
	w.Arr()
	w.EndArr()
	w.Key("obj")
	w.Obj()
	w.EndObj()
	w.EndObj()
	require.JSONEq(t, `{"arr":[],"obj":{}}`, string(w.Bytes()))
}

func TestJSONWriter_DeepNesting_NoPanic(t *testing.T) {
	w := newJSONWriter()
	depth := 70
	for i := 0; i < depth; i++ {
		w.Obj()
		w.Key("n")
	}
	w.Str("leaf")
	for i := 0; i < depth; i++ {
		w.EndObj()
	}
	got := w.Bytes()
	require.True(t, len(got) > 0, "should produce output without panic")
}

func TestJSONWriter_StringEscaping(t *testing.T) {
	w := newJSONWriter()
	w.Obj()
	w.Key("q")
	w.Str(`say "hello"`)
	w.Key("bs")
	w.Str(`back\slash`)
	w.Key("nl")
	w.Str("line1\nline2")
	w.EndObj()

	got := string(w.Bytes())
	require.JSONEq(t, `{"q":"say \"hello\"","bs":"back\\slash","nl":"line1\nline2"}`, got)
}
