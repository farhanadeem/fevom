package machines

import "testing"

func TestTheAgentsMacPathIsBoundOnlyWhereTheMachineHasTheMacs(t *testing.T) {
	for p, want := range map[string]bool{
		"/private/tmp/com.apple.launchd.x/Listeners":                  true,
		"/Users/me/Library/Group Containers/x.1password/t/agent.sock": true,
		"/Volumes/Keys/agent.sock":                                    true,
		"/tmp/ssh-x/agent.1":                                          false,
		"/home/me/agent.sock":                                         false,
		"/private":                                                    false,
		"/Usersx/agent.sock":                                          false,
		"/private/../etc/x":                                           false,
		"":                                                            false,
	} {
		if got := agentPathInMachine(p); got != want {
			t.Errorf("%q: %v", p, got)
		}
	}
}
