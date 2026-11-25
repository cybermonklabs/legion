package main

import "testing"

// TestMachineLoginNamesThePageJoinedOntoTheApproveURL: machine login names the page
// /credentials/machine joined onto AGENT_SECRETS_APPROVE_URL's path (machinePage). An approve URL
// with a fragment (fragment) names the page under its path, where the setting's text with the
// route written after it put the route inside the fragment and the line printed no page at all;
// one with a path (path) names the page under that path, as it did. Both name the setting.
func TestMachineLoginNamesThePageJoinedOntoTheApproveURL(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelperAt(t, "KQ7M-X4PZ", "https://broker.example/credentials/machine", []string{"issued"})
	for _, tc := range []struct{ name, approve string }{
		{"fragment", "https://dispatch.example/app#settings"},
		{"path", "https://dispatch.example/app/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
				[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_APPROVE_URL=" + tc.approve}, "machine", "login")
			want := "machine login code: KQ7M-X4PZ\nenter it at https://dispatch.example/app/credentials/machine (AGENT_SECRETS_APPROVE_URL names its origin) — approve only if the code matches this terminal\n"
			if exit != 0 || stdout != want {
				t.Errorf("AGENT_SECRETS_APPROVE_URL=%q: exit %d stdout %q stderr %q; want %q", tc.approve, exit, stdout, stderr, want)
			}
		})
	}
}
