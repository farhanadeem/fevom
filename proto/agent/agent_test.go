package agent

import (
	"testing"

	"fev/proto/contract"
)

func TestFixturesMatchTheSchema(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range contract.Verify(fixtures, Schema) {
		t.Error(err)
	}
}
