package vmmctl

import (
	"embed"

	"fev/proto/contract"
)

//go:embed fixtures/*.json
var fixtureFiles embed.FS

//go:embed fixtures/wire/session.jsonl
var wireSession []byte

type Fixture = contract.Fixture

func Fixtures() ([]Fixture, error) { return contract.Load(fixtureFiles, "fixtures") }

var Schema = map[string]contract.Types{
	MethodPause:  {Empty{}, PauseResult{}},
	MethodResume: {Empty{}, ResumeResult{}},
	MethodStats:  {Empty{}, Stats{}},
	MethodStop:   {Empty{}, Empty{}},
	MethodKill:   {Empty{}, Empty{}},
	MethodBudget: {Baseline{}, BudgetResult{}},
	NotifyReset:  {Reset{}, nil},
	NotifySpace:  {Space{}, nil},
}

func WireSession() []byte { return wireSession }

func Conforms(data []byte, zero any) error { return contract.Conforms(data, zero) }
