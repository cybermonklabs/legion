package chips

import "testing"

// TestABareKeyAReadReturnedIsLinked: the preview at e2b341f7 answered "DEMO-21 is a P3 backlog
// feature with no assignee" with DEMO-21 as text, so the bar drew no chip of it. LinkBareKeys
// writes each key that stands bare in an answer's text, and that a read of the run returned, as
// the link to its issue, whatever punctuation follows it; a key no read returned stays text. A
// key in a link's words or its href, in an autolink, a picture, a code span or a fenced block, in
// a reference or an address, and a longer key stay as written. Each answer also holds a key that
// is linked, so no case passes with no rewrite, and LinkBareKeys of what it wrote is what it
// wrote.
func TestABareKeyAReadReturnedIsLinked(t *testing.T) {
	returned := map[string]bool{"DEMO-3": true, "DEMO-21": true}
	for _, c := range []struct{ name, answer, want string }{
		{
			"a key a read returned",
			"DEMO-21 is a P3 backlog feature with no assignee.",
			"[DEMO-21](dispatch://DEMO-21) is a P3 backlog feature with no assignee.",
		},
		{
			"a key no read returned",
			"DEMO-4 waits on DEMO-21, and ACME-9 on DEMO-4.",
			"DEMO-4 waits on [DEMO-21](dispatch://DEMO-21), and ACME-9 on DEMO-4.",
		},
		{
			"punctuation after a key",
			"Read DEMO-3. Then DEMO-3, (DEMO-3) and **DEMO-3**; DEMO-3: done, and \"DEMO-3\"!\nDEMO-3",
			"Read [DEMO-3](dispatch://DEMO-3). Then [DEMO-3](dispatch://DEMO-3), ([DEMO-3](dispatch://DEMO-3)) and " +
				"**[DEMO-3](dispatch://DEMO-3)**; [DEMO-3](dispatch://DEMO-3): done, and \"[DEMO-3](dispatch://DEMO-3)\"!\n" +
				"[DEMO-3](dispatch://DEMO-3)",
		},
		{
			"a key in a link, an autolink or a picture",
			"[DEMO-3 is blocked](dispatch://DEMO-21), [the spec](dispatch://DEMO-3/spec), [DEMO-3](dispatch://DEMO-3), " +
				"[the page](/issues/DEMO-3 \"DEMO-3\"), <dispatch://DEMO-3> and ![DEMO-3](https://dispatch.example/DEMO-3.png) stay beside DEMO-21.",
			"[DEMO-3 is blocked](dispatch://DEMO-21), [the spec](dispatch://DEMO-3/spec), [DEMO-3](dispatch://DEMO-3), " +
				"[the page](/issues/DEMO-3 \"DEMO-3\"), <dispatch://DEMO-3> and ![DEMO-3](https://dispatch.example/DEMO-3.png) stay beside [DEMO-21](dispatch://DEMO-21).",
		},
		{
			"a key in code",
			"`DEMO-3` and ``a `DEMO-3` b`` and\n```\nDEMO-3 was read\n```\n~~~\nDEMO-3\n~~~\nstay beside DEMO-21.",
			"`DEMO-3` and ``a `DEMO-3` b`` and\n```\nDEMO-3 was read\n```\n~~~\nDEMO-3\n~~~\nstay beside [DEMO-21](dispatch://DEMO-21).",
		},
		{
			"a key in a reference, an address, a name or a longer key",
			"DEMO-3/spec, dispatch://DEMO-3, https://dispatch.example/issues/DEMO-3, $DEMO-3, #DEMO-3, @DEMO-3, " +
				"a_DEMO-3, DEMO-3_b, x.DEMO-3, DEMO-3-b, DEMO-30, XDEMO-3, DEMO-3x and 4DEMO-3 stay beside DEMO-21.",
			"DEMO-3/spec, dispatch://DEMO-3, https://dispatch.example/issues/DEMO-3, $DEMO-3, #DEMO-3, @DEMO-3, " +
				"a_DEMO-3, DEMO-3_b, x.DEMO-3, DEMO-3-b, DEMO-30, XDEMO-3, DEMO-3x and 4DEMO-3 stay beside [DEMO-21](dispatch://DEMO-21).",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			read := func(key string) bool { return returned[key] }
			got := LinkBareKeys(c.answer, read)
			if got != c.want {
				t.Fatalf("LinkBareKeys(%q)\n= %q\nwant %q", c.answer, got, c.want)
			}
			if again := LinkBareKeys(got, read); again != got {
				t.Errorf("LinkBareKeys of what it wrote, %q, is %q", got, again)
			}
		})
	}
}

// TestAResultHoldsAKeyAsAWholeToken: the server links a bare key only where a tool result of the
// run holds it, as a whole token, as LinkBareKeys reads one: a key in a JSON string or a list
// does, one inside a longer key, a reference or an address does not.
func TestAResultHoldsAKeyAsAWholeToken(t *testing.T) {
	for text, holds := range map[string]bool{
		`{"key": "DEMO-3", "title": "Export"}`:   true,
		"DEMO-30, DEMO-3":                        true,
		"DEMO-30 and XDEMO-3":                    false,
		"dispatch://DEMO-3/spec":                 false,
		"https://dispatch.example/issues/DEMO-3": false,
		"":                                       false,
	} {
		if got := HoldsKey(text, "DEMO-3"); got != holds {
			t.Errorf("HoldsKey(%q, DEMO-3) = %v, want %v", text, got, holds)
		}
	}
}
