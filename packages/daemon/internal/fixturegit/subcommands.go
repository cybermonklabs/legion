package fixturegit

// Subcommand is one git argument list and where git finds its subcommand in it: Sub is the index in
// Args of the first word that is no global option, or -1 when Args has none. git reads the global
// options as handle_options (git.c) of git 2.47.3 reads them. The value of -C, -c, --git-dir,
// --work-tree, --namespace, --config-env, --attr-source and --shallow-file is the next word. Every
// other word that starts with `-` is an option of its own, the `--opt=value` spellings included.
// --help, -h, --version, -v, --html-path, --man-path, --info-path, and a word that starts with
// --exec-path and has no `=` after it, end the search with none: git runs its help or version
// command, or prints a path and ends.
type Subcommand struct {
	Args []string
	Sub  int
}

// Subcommands is the table that holds Legion's two readers of a git command's subcommand to one
// answer: gitSubcommand of `legion git` (packages/daemon/cmd/legion/git.go) and the pod's git
// wrapper (packages/daemon/docker/git). Tests of both read it. It holds plain commands, each global
// option that takes the next word as its value in front of a commit and a push, with the other
// subcommand's name as that value, the `--opt=value` spellings, and the words that end the search.
// Each call returns a new copy.
func Subcommands() []Subcommand {
	return []Subcommand{
		{[]string{"commit", "-m", "x"}, 0},
		{[]string{"push", "origin", "main", "--no-verify"}, 0},
		{[]string{"status", "--short"}, 0},
		{[]string{"send-pack", "origin", "x"}, 0},
		{[]string{"rev-list", "push", "commit"}, 0},

		{[]string{"-C", "push", "commit", "-m", "x"}, 2},
		{[]string{"-C", "commit", "push", "origin"}, 2},
		{[]string{"-c", "push", "commit", "-m", "x"}, 2},
		{[]string{"-c", "commit", "push", "origin"}, 2},
		{[]string{"--git-dir", "push", "commit", "-m", "x"}, 2},
		{[]string{"--git-dir", "commit", "push", "origin"}, 2},
		{[]string{"--work-tree", "push", "commit", "-m", "x"}, 2},
		{[]string{"--work-tree", "commit", "push", "origin"}, 2},
		{[]string{"--namespace", "push", "commit", "-m", "x"}, 2},
		{[]string{"--namespace", "commit", "push", "origin"}, 2},
		{[]string{"--config-env", "push", "commit", "-m", "x"}, 2},
		{[]string{"--config-env", "commit", "push", "origin"}, 2},
		{[]string{"--attr-source", "push", "commit", "-m", "x"}, 2},
		{[]string{"--attr-source", "commit", "push", "origin"}, 2},
		{[]string{"--shallow-file", "push", "commit", "-m", "x"}, 2},
		{[]string{"--shallow-file", "commit", "push", "origin"}, 2},
		{[]string{"-C", "/tmp/repo", "-c", "a=b", "--git-dir", "--no-verify", "push", "--no-verify", "origin"}, 6},
		{[]string{"-C"}, -1},
		{[]string{"--shallow-file"}, -1},

		{[]string{"--git-dir=/tmp/repo/.git", "commit", "-m", "x"}, 1},
		{[]string{"--work-tree=/tmp/repo", "push"}, 1},
		{[]string{"--namespace=ns", "commit"}, 1},
		{[]string{"--config-env=core.editor=EDITOR", "push"}, 1},
		{[]string{"--attr-source=HEAD", "commit"}, 1},
		{[]string{"--shallow-file=/tmp/shallow", "push"}, 1},
		{[]string{"--exec-path=/tmp/libexec", "commit"}, 1},
		{[]string{"--no-pager", "-p", "--bare", "push"}, 3},

		{[]string{"--help", "commit", "-m", "x"}, -1},
		{[]string{"--help", "push", "--no-verify"}, -1},
		{[]string{"-C", "/tmp/repo", "--help", "commit", "-m", "x"}, -1},
		{[]string{"-h", "commit"}, -1},
		{[]string{"-h", "push"}, -1},
		{[]string{"--version", "commit"}, -1},
		{[]string{"--version", "push"}, -1},
		{[]string{"-v", "commit"}, -1},
		{[]string{"-v", "push"}, -1},
		{[]string{"--exec-path", "push"}, -1},
		{[]string{"-C", "/", "--exec-path", "commit"}, -1},
		{[]string{"--exec-pathx", "commit"}, -1},
		{[]string{"--html-path", "commit"}, -1},
		{[]string{"--html-path", "push"}, -1},
		{[]string{"--man-path", "commit"}, -1},
		{[]string{"--man-path", "push"}, -1},
		{[]string{"--info-path", "commit"}, -1},
		{[]string{"--info-path", "push"}, -1},
	}
}
