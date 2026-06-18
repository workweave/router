package admin

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsEnvKeyed_TrueWhenEnvVarSet(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-value")
	assert.True(t, isEnvKeyed("anthropic"))
}

func TestIsEnvKeyed_FalseWhenEnvVarUnset(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	assert.False(t, isEnvKeyed("openai"))
}

func TestIsEnvKeyed_FalseForUnknownProvider(t *testing.T) {
	assert.False(t, isEnvKeyed("not-a-real-provider"))
}
