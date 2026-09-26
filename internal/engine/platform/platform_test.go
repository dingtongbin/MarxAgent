// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A prefix test is the classic way to write a path check that passes when it should
// refuse, because a sibling directory shares a prefix with the directory it is
// supposed to be inside. This is the case that catches it.
func TestAPathOutsideTheRootIsRefused(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "data")
	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"the root itself", root, true},
		{"a file inside", filepath.Join(root, "notes.txt"), true},
		{"a directory inside", filepath.Join(root, "sub", "deep"), true},
		{"a sibling sharing the prefix", filepath.Join(string(filepath.Separator), "srv", "data-archive"), false},
		{"a sibling sharing a longer prefix", filepath.Join(string(filepath.Separator), "srv", "database"), false},
		{"the parent", filepath.Dir(root), false},
		{"climbing back out", filepath.Join(root, "..", "other"), false},
		{"climbing out from deep inside", filepath.Join(root, "sub", "..", "..", "etc"), false},
		{"a dotdot that is only a name", filepath.Join(root, "..hidden"), true},
		{"a path with a dot in a name", filepath.Join(root, "a.txt"), true},
		{"nothing at all", "", false},
		{"no root at all", "", false},
	}
	for _, testCase := range cases {
		got := PathWithin(root, testCase.target)
		if got != testCase.want {
			t.Fatalf("%s: PathWithin(%q) is %v, want %v", testCase.name, testCase.target, got, testCase.want)
		}
	}
}

// Two different spellings of the same path are the same path, and a confinement check
// that says otherwise would refuse a file the caller plainly named.
func TestTheSamePathSpelledTwoWaysIsTheSamePath(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "data")
	messy := filepath.Join(root, "sub", "..", "notes.txt")
	if !PathWithin(root, messy) {
		t.Fatalf("%q is inside %q but was refused", messy, root)
	}
	if !EqualPath(filepath.Join(root, "notes.txt"), messy) {
		t.Fatal("two spellings of one path were called different")
	}
	if EqualPath(filepath.Join(root, "notes.txt"), filepath.Join(root, "other.txt")) {
		t.Fatal("two different files were called the same")
	}
	if CleanPath(messy) != filepath.Join(root, "notes.txt") {
		t.Fatalf("cleaning gave %q", CleanPath(messy))
	}
	// The model writes forward slashes whatever the host uses, so a path that came
	// from a model has to be readable by a host that does not use them.
	slashed := filepath.ToSlash(filepath.Join(root, "notes.txt"))
	if !PathWithin(root, filepath.FromSlash(slashed)) {
		t.Fatalf("the host form of %q was refused", slashed)
	}
	if FromSlash(ToSlash(root)) != root {
		t.Fatal("a round trip through slashes changed the path")
	}
}

// The host's own rules about case are the platform package's business, and a caller
// asking about them deserves the truth rather than an assumption.
func TestWhatThisHostDoesAboutCase(t *testing.T) {
	if runtime.GOOS == "windows" {
		if !CaselessNames() {
			t.Fatal("this host does distinguish case")
		}
		// A check that found these different would refuse a path the caller meant, and
		// a caller that keeps being refused stops trusting the check.
		root := `C:\Work`
		if !PathWithin(root, `c:\work\notes.txt`) {
			t.Fatal("a path differing only in case was called outside")
		}
		return
	}
	if CaselessNames() {
		t.Fatal("this host does not distinguish case")
	}
	// Here they really are two places, and folding them would be a lie a confinement
	// check would then rely on.
	root := filepath.Join(string(filepath.Separator), "srv", "Data")
	if PathWithin(root, filepath.Join(string(filepath.Separator), "srv", "data")) {
		t.Fatal("two different directories were called the same")
	}
}

// The application keeps its files wherever the host says, and no path is written down
// here, because a path written here would be wrong on two of the three systems.
func TestTheApplicationDirectoriesComeFromTheHost(t *testing.T) {
	config, err := AppConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(config) != AppName {
		t.Fatalf("the config directory is %q, which is not under %q", config, AppName)
	}
	data, err := AppDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if data != config {
		t.Fatalf("the data directory is %q and the config directory is %q", data, config)
	}
	cache, err := AppCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cache) != AppName {
		t.Fatalf("the cache directory is %q", cache)
	}
	// Both are under a host directory rather than somewhere invented.
	hostConfig, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !PathWithin(hostConfig, config) {
		t.Fatalf("the config directory %q is not under the host's %q", config, hostConfig)
	}
}

// A file rewritten with the other ending is a change nobody asked for, so an editor
// has to be able to find out which ending a file already uses and put it back.
func TestALineEndingIsFoundAndKept(t *testing.T) {
	cases := []struct {
		name string
		body string
		want LineEnding
	}{
		{"unix", "one\ntwo\nthree", LF},
		{"windows", "one\r\ntwo\r\nthree", CRLF},
		{"a single line with no ending", "one", LF},
		{"nothing at all", "", LF},
		{"mixed, decided by the first", "one\r\ntwo\nthree", CRLF},
	}
	dir := t.TempDir()
	for _, testCase := range cases {
		path := filepath.Join(dir, "case.txt")
		if err := os.WriteFile(path, []byte(testCase.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := DetectLineEnding(path)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if got.Name != testCase.want.Name {
			t.Fatalf("%s: the ending is %q, want %q", testCase.name, got.Name, testCase.want.Name)
		}
		// The same question asked of the bytes gives the same answer, which is what
		// makes it safe to use on text that is about to be written.
		if fromBytes := DetectLineEndingIn([]byte(testCase.body)); fromBytes.Name != testCase.want.Name {
			t.Fatalf("%s: the bytes say %q and the file says %q",
				testCase.name, fromBytes.Name, got.Name)
		}
	}
	// Putting an ending back produces that ending throughout, whatever came in.
	if got := ApplyLineEnding("one\ntwo", CRLF); got != "one\r\ntwo" {
		t.Fatalf("applying crlf gave %q", got)
	}
	if got := ApplyLineEnding("one\r\ntwo", LF); got != "one\ntwo" {
		t.Fatalf("applying lf gave %q", got)
	}
	// Text that already mixes comes out uniform rather than gaining a second ending.
	// Three lines have two endings between them. A count that expected three would be
	// counting lines and calling them endings.
	if got := ApplyLineEnding("one\r\ntwo\nthree", CRLF); strings.Count(got, "\r\n") != 2 {
		t.Fatalf("applying crlf to mixed text gave %q", got)
	}
	if got := ApplyLineEnding("one\r\ntwo\nthree", LF); strings.Contains(got, "\r") {
		t.Fatalf("applying lf left a carriage return: %q", got)
	}
	if _, err := DetectLineEnding(filepath.Join(dir, "no-such-file")); err == nil {
		t.Fatal("the ending of a file that is not there was reported")
	}
}

// Reading through a link is what links are for. Writing through one puts bytes
// somewhere the caller was not told about, so it is refused, and the refusal says
// where the link was aiming.
func TestALinkIsReadThroughAndNeverWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("real"), DefaultFileMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host will not make a symbolic link: %v", err)
	}
	isLink, err := IsLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if !isLink {
		t.Fatal("a link was not reported as one")
	}
	if plain, err := IsLink(target); err != nil || plain {
		t.Fatalf("an ordinary file was reported as a link: %v", err)
	}
	if _, err := IsLink(filepath.Join(dir, "no-such-file")); err == nil {
		t.Fatal("a file that is not there was inspected without complaint")
	}
	// The bytes go where the link points, or nowhere at all, but never silently
	// somewhere else.
	err = WriteFile(link, []byte("through the link"), DefaultFileMode)
	if !errors.Is(err, ErrSymlinkForbidden) {
		t.Fatalf("writing through a link was allowed: %v", err)
	}
	if aiming, err := ReadLinkTarget(link); err != nil || aiming != target {
		t.Fatalf("the link aims at %q, want %q (%v)", aiming, target, err)
	}
	// Writing where there is no link is the ordinary case and has to work.
	if err := WriteFile(target, []byte("direct"), DefaultFileMode); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "direct" {
		t.Fatalf("the direct write did not land: %q (%v)", data, err)
	}
	if _, err := ReadLinkTarget(target); err == nil {
		t.Fatal("an ordinary file was read as a link")
	}
}

// A file is created private, because a session transcript and an audit ledger are the
// user's and not every account on a shared machine should read them.
func TestFilesAreCreatedPrivateAndDirectoriesToo(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	if err := EnsureDir(nested); err != nil {
		t.Fatal(err)
	}
	// Asking again is not a failure: a directory that is already there is the state
	// the caller asked for.
	if err := EnsureDir(nested); err != nil {
		t.Fatalf("creating a directory that exists: %v", err)
	}
	if err := CreateDirs(filepath.Join(dir, "x"), filepath.Join(dir, "y")); err != nil {
		t.Fatal(err)
	}
	// A list is taken as a whole, so a caller making several either gets told which
	// one failed or gets them all.
	if err := CreateDirs(filepath.Join(dir, "z"), ""); err == nil {
		t.Fatal("an empty path in a list was accepted")
	}
	path := filepath.Join(dir, "private.txt")
	if err := os.WriteFile(path, []byte("secret"), DefaultFileMode); err != nil {
		t.Fatal(err)
	}
	if err := SetFileMode(path, DefaultFileMode); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Fatalf("the file is %v, which others can read", mode)
		}
	}
	// A path that cannot be a file is reported rather than created.
	if err := SetFileMode(filepath.Join(dir, "no-such-file"), 0o600); err == nil {
		t.Fatal("the mode of a file that is not there was set")
	}
}

// A link inside a confined directory can point anywhere, so a check that does not
// follow it would call it inside when it is not.
func TestAConfinedPathIsCheckedAfterFollowingLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside"), DefaultFileMode); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "sub")
	if err := EnsureDir(inside); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(inside, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("this host will not make a symbolic link: %v", err)
	}
	// By name it is inside. By where it actually leads, it is not, and the second
	// answer is the one that protects anything.
	if !PathWithin(root, link) {
		t.Fatal("a link inside the root was not inside it by name")
	}
	allowed, err := ResolveWithin(root, link)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("a link out of the root was allowed once followed")
	}
	// A file that is not there yet is asked about by where it would land, because
	// that is the question a caller creating it is asking.
	fresh := filepath.Join(inside, "new.txt")
	allowed, err = ResolveWithin(root, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("a file about to be created inside the root was refused")
	}
	// And one about to be created outside it is still outside it.
	allowed, err = ResolveWithin(root, filepath.Join(outside, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("a file about to be created outside the root was allowed")
	}
	// A root that cannot be resolved is a refusal, because a path whose real location
	// is unknown has not been shown to be anywhere in particular.
	if _, err := ResolveWithin(filepath.Join(root, "no-such-root"), inside); err == nil {
		t.Fatal("a root that is not there was resolved")
	}
}

// The root is very often reached through a link itself — a temporary directory on
// macOS is — so comparing a resolved target against an unresolved root would refuse
// everything.
func TestARootReachedThroughALinkStillContainsItsOwnFiles(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := EnsureDir(real); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("this host will not make a symbolic link: %v", err)
	}
	allowed, err := ResolveWithin(alias, filepath.Join(real, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("a file inside the root was refused because the root was a link")
	}
}

// Every system is built for and every processor is built everywhere, so the promised
// list is a rectangle rather than the cases that happened to be convenient.
func TestThePromisedBuildTargetsAreARectangle(t *testing.T) {
	systems := map[string]bool{}
	processors := map[string]bool{}
	for _, target := range Targets {
		systems[target.GOOS] = true
		processors[target.GOARCH] = true
		if target.String() != target.GOOS+"/"+target.GOARCH {
			t.Fatalf("the target reads as %q", target.String())
		}
		env := target.Env()
		if env["CGO_ENABLED"] != "0" {
			t.Fatalf("%s leaves cgo on", target)
		}
		if env["GOOS"] != target.GOOS || env["GOARCH"] != target.GOARCH {
			t.Fatalf("%s carries the wrong environment: %v", target, env)
		}
		// The command has to be runnable as it stands, so the environment is spelled
		// out with it rather than left to the caller to remember.
		command := strings.Join(BuildCommand(target), " ")
		if !strings.Contains(command, "GOARCH="+target.GOARCH) {
			t.Fatalf("the command for %s does not carry the target: %q", target, command)
		}
		if !strings.Contains(command, "CGO_ENABLED=0") {
			t.Fatalf("the command for %s does not turn cgo off: %q", target, command)
		}
	}
	if len(systems) != 3 {
		t.Fatalf("%d systems are built for, want three", len(systems))
	}
	if len(processors) != 2 {
		t.Fatalf("%d processors are built for, want two", len(processors))
	}
	// A missing combination is reported by name, because "some are missing" is not
	// something a caller can act on.
	if err := VerifyTargets(Targets); err != nil {
		t.Fatal(err)
	}
	partial := Targets[:len(Targets)-1]
	err := VerifyTargets(partial)
	if err == nil {
		t.Fatal("a missing target went unnoticed")
	}
	for _, target := range Targets[len(Targets)-1:] {
		if !strings.Contains(err.Error(), target.String()) {
			t.Fatalf("the complaint does not name %s: %v", target, err)
		}
	}
	// The order a command comes out in is fixed, so a log of them can be read.
	first := strings.Join(BuildCommand(Targets[0]), " ")
	for range 5 {
		if again := strings.Join(BuildCommand(Targets[0]), " "); again != first {
			t.Fatalf("the command changed between calls: %q then %q", first, again)
		}
	}
}

// A shell that cannot be found is reported, never run with an empty command, and a
// shell the caller named wins over the one this host would have picked.
func TestTheShellIsOneThatCanActuallyBeRun(t *testing.T) {
	choice := DefaultShell()
	if !choice.Usable() {
		t.Fatalf("this host has no shell: %v", choice.Err)
	}
	if choice.Path == "" {
		t.Fatal("a usable shell has no path")
	}
	if !choice.Interactive {
		t.Fatal("the shell cannot take a command from a string")
	}
	args := choice.Args("echo hello")
	if len(args) < 2 {
		t.Fatalf("the arguments are %v", args)
	}
	if args[0] != choice.Path {
		t.Fatalf("the arguments start with %q, not the shell %q", args[0], choice.Path)
	}
	if args[len(args)-1] != "echo hello" {
		t.Fatalf("the script is not the last argument: %v", args)
	}
	// A shell that is not there is a question a caller can ask before it has work to
	// do, which is the point at which knowing is still cheap.
	missing := ShellChoice{}
	if missing.Usable() {
		t.Fatal("a shell that is not there was reported as usable")
	}
	// A shell that cannot be run can always say why, including one that was never
	// filled in. A caller that wrapped nothing would produce a complaint with a hole
	// in it, which is worse than no complaint.
	if !errors.Is(missing.Reason(), ErrNoShell) {
		t.Fatalf("an empty choice says %v", missing.Reason())
	}
	if missing.Reason() == nil {
		t.Fatal("an unusable shell did not say why")
	}
	// One that brought its own reason keeps it, because it knows more than the empty
	// case does.
	own := ShellChoice{Err: errors.New("looked in three places")}
	if own.Usable() {
		t.Fatal("a choice that failed was reported as usable")
	}
	if own.Reason() == nil || !strings.Contains(own.Reason().Error(), "three places") {
		t.Fatalf("a choice lost its own reason: %v", own.Reason())
	}
	// And a usable shell has no complaint about it at all.
	if choice.Reason() != nil {
		t.Fatalf("a usable shell complained: %v", choice.Reason())
	}
	// A caller that names its own shell is believed, which is how a caller asks for
	// something this host would not have chosen.
	named := ShellChoice{Path: "my-shell", Interactive: true}
	if !named.Usable() {
		t.Fatal("a shell the caller named was refused")
	}
	if args := named.Args("x"); args[0] != "my-shell" {
		t.Fatalf("the arguments are %v", args)
	}
}

// A host with no shell at all is reported by name, and the answer is the same whether
// it was found missing by preference or by a PATH somebody emptied.
func TestAHostWithNoShellSaysWhatItLookedFor(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "nothing-here"))
	choice := DefaultShell()
	if choice.Usable() {
		t.Skip("this host has a shell somewhere the empty PATH did not hide")
	}
	if choice.Reason() == nil {
		t.Fatal("a host with no shell did not say so")
	}
	// A caller that names its own shell is unaffected by the host having none, which
	// is how a caller with a shell the host cannot find carries on.
	named := ShellChoice{Path: "my-shell", Interactive: true}
	if !named.Usable() {
		t.Fatal("a shell the caller named was refused on a host with none")
	}
}

// A choice that cannot be run says why whichever way it is broken.
func TestEveryUnusableShellCanExplainItself(t *testing.T) {
	// A path with an error is a shell that was tried and failed.
	tried := ShellChoice{Path: "/bin/false", Err: errors.New("it will not start")}
	if tried.Usable() {
		t.Fatal("a shell with an error was reported as usable")
	}
	if !strings.Contains(tried.Reason().Error(), "will not start") {
		t.Fatalf("the reason was lost: %v", tried.Reason())
	}
	// A path with no error and no path is a shell nobody filled in.
	blank := ShellChoice{}
	if !errors.Is(blank.Reason(), ErrNoShell) {
		t.Fatalf("a blank choice says %v", blank.Reason())
	}
}

// A host that cannot say where an application keeps its files has to say that rather
// than return somewhere invented, because a caller would write to the invented place.
func TestAHostWithNoApplicationDirectorySaysSo(t *testing.T) {
	// Windows reads this one; the others read their own, and clearing whichever the
	// host uses is what makes it unavailable.
	for _, name := range []string{"APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "HOME"} {
		t.Setenv(name, "")
	}
	config, err := AppConfigDir()
	cache, cacheErr := AppCacheDir()
	if err == nil && cacheErr == nil {
		t.Skip("this host found an application directory anyway")
	}
	// Whatever came back, it is not a half-made path with the application name
	// hanging off the end of nothing.
	if err == nil && filepath.Base(config) != AppName {
		t.Fatalf("the config directory is %q", config)
	}
	if cacheErr == nil && filepath.Base(cache) != AppName {
		t.Fatalf("the cache directory is %q", cache)
	}
}

// A file that cannot be written is reported. A caller that was told a write succeeded
// and then found nothing there would have no way to know it had been lied to.
func TestAFileThatCannotBeWrittenIsReported(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a file rather than a directory cannot be written, on any
	// host, and says so rather than failing later.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(blocker, "child.txt"), []byte("y"), 0o600); err == nil {
		t.Fatal("a file was written underneath something that is not a directory")
	}
}

// A path whose every part is missing all the way up is still answered, by appending to
// the nearest thing that does exist, so that a caller about to create a deep tree is
// not refused for the tree not being there yet.
func TestAPathThatDoesNotExistYetIsStillPlaced(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "not", "there", "yet", "file.txt")
	allowed, err := ResolveWithin(root, deep)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("a file under the root was refused for not existing yet")
	}
	// And a path that climbs out on its way down is still refused.
	climbing := filepath.Join(root, "not", "..", "..", "elsewhere.txt")
	allowed, err = ResolveWithin(root, climbing)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("a path that climbs out of the root was allowed")
	}
}

// A mode is what the caller asked for, and what it means is the host's business. The
// only thing that must hold everywhere is that a caller is not told a mode was applied
// when it was not.
func TestAModeIsAppliedAndReportedHonestly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "modes.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetFileMode(path, fs.FileMode(0o600)); err != nil {
		t.Fatal(err)
	}
	// Read only is asked for as well as writable, because it is the one a caller
	// reaches for when a file must not be overwritten by accident. Both directions
	// have to work, because a host that can only add permission and never take it
	// away is a host where "read only" quietly does not happen.
	if err := SetFileMode(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := SetFileMode(path, DefaultFileMode); err != nil {
		t.Fatalf("making a file writable again: %v", err)
	}
	if runtime.GOOS == "windows" {
		// There are no permission bits here, so the most that can be done is the one
		// attribute there is. Saying so is better than pretending.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o200 != 0 {
			t.Skip("this host reports a writable file whatever the mode says")
		}
		return
	}
	if err := SetFileMode(path, 0o400); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o400 {
		t.Fatalf("the mode is %v, want 0400", mode)
	}
}
