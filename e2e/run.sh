#!/bin/sh

set -eu

THREAD_KEEP=${THREAD_KEEP:-/usr/local/bin/thread-keep}
THREAD_KEEP_SERVER=${THREAD_KEEP_SERVER:-/usr/local/bin/thread-keep-server}
THREAD_KEEP_E2E_FAKEGITHUB=${THREAD_KEEP_E2E_FAKEGITHUB:-/usr/local/bin/thread-keep-e2e-fakegithub}
THREAD_KEEP_E2E_MCPCLIENT=${THREAD_KEEP_E2E_MCPCLIENT:-/usr/local/bin/thread-keep-e2e-mcpclient}
WORKDIR=$(mktemp -d)
SERVER_PID=""
FAKE_GITHUB_PID=""
CLUSTER_A_PID=""
CLUSTER_B_PID=""

cleanup() {
	[ -z "$SERVER_PID" ] || kill "$SERVER_PID" 2>/dev/null || true
	[ -z "$CLUSTER_A_PID" ] || kill "$CLUSTER_A_PID" 2>/dev/null || true
	[ -z "$CLUSTER_B_PID" ] || kill "$CLUSTER_B_PID" 2>/dev/null || true
	[ -z "$FAKE_GITHUB_PID" ] || kill "$FAKE_GITHUB_PID" 2>/dev/null || true
	rm -rf "$WORKDIR"
}
trap cleanup EXIT INT TERM

fail() {
	echo "E2E failure: $*" >&2
	exit 1
}

assert_contains() {
	file=$1
	needle=$2
	grep -F -- "$needle" "$file" >/dev/null || fail "expected $file to contain $needle: $(cat "$file")"
}

assert_empty() {
	[ ! -s "$1" ] || fail "expected $1 to be empty: $(cat "$1")"
}

assert_file() {
	[ -f "$1" ] || fail "expected file $1"
}

capture() {
	LAST_STDOUT=$(mktemp "$WORKDIR/stdout.XXXXXX")
	LAST_STDERR=$(mktemp "$WORKDIR/stderr.XXXXXX")
	set +e
	"$@" >"$LAST_STDOUT" 2>"$LAST_STDERR"
	LAST_STATUS=$?
	set -e
}

expect_exit() {
	expected=$1
	shift
	capture "$@"
	[ "$LAST_STATUS" -eq "$expected" ] || fail "expected exit $expected, got $LAST_STATUS; stdout: $(cat "$LAST_STDOUT"); stderr: $(cat "$LAST_STDERR")"
}

expect_json_error() {
	expected=$1
	error_code=$2
	shift 2
	expect_exit "$expected" "$@"
	assert_empty "$LAST_STDOUT"
	assert_contains "$LAST_STDERR" '"version":1'
	assert_contains "$LAST_STDERR" "\"code\":\"$error_code\""
}

run() {
	capture "$@"
	[ "$LAST_STATUS" -eq 0 ] || fail "command failed with exit $LAST_STATUS; stdout: $(cat "$LAST_STDOUT"); stderr: $(cat "$LAST_STDERR")"
	assert_empty "$LAST_STDERR"
}

wait_for_listen() {
	log_file=$1
	attempts=0
	until grep -q "listening on" "$log_file" 2>/dev/null; do
		attempts=$((attempts + 1))
		[ "$attempts" -le 100 ] || fail "server did not report listening: $(cat "$log_file" 2>/dev/null)"
		sleep 0.1
	done
}

[ -n "$THREAD_KEEP" ] || fail "THREAD_KEEP must not be empty"

[ -x "$THREAD_KEEP" ] || fail "thread-keep binary is not executable: $THREAD_KEEP"

[ -d "$WORKDIR" ] || fail "temporary work directory is unavailable"

init_repo() {
	repo=$1
	mkdir -p "$repo"
	git -C "$repo" init -q -b main
	git -C "$repo" config user.name "Thread Keep E2E"
	git -C "$repo" config user.email "thread-keep-e2e@example.test"
	cat >"$repo/sample.go" <<'EOF'
package sample

type Worker struct{}

func Run() {}

func (Worker) Execute() {}
EOF
	git -C "$repo" add sample.go
	git -C "$repo" commit -qm "initial source"
}

add_typescript_source() {
	repo=$1
	mkdir -p "$repo/web"
	cat >"$repo/web/app.ts" <<'EOF'
export interface User { id: string }
export class Service { run(): void {} }
export const helper = () => 1
EOF
	git -C "$repo" add web/app.ts
	git -C "$repo" commit -qm "add TypeScript source"
}

add_javascript_source() {
	repo=$1
	mkdir -p "$repo/web"
	cat >"$repo/web/app.js" <<'EOF'
export class Service { run() {} }
export const helper = () => 1
EOF
	git -C "$repo" add web/app.js
	git -C "$repo" commit -qm "add JavaScript source"
}

add_python_source() {
	repo=$1
	mkdir -p "$repo/services"
	cat >"$repo/services/app.py" <<'EOF'
class Service:
    def run(self):
        return None

def helper():
    return 1
EOF
	git -C "$repo" add services/app.py
	git -C "$repo" commit -qm "add Python source"
}

add_java_source() {
	repo=$1
	mkdir -p "$repo/src"
	cat >"$repo/src/Service.java" <<'EOF'
class Service {
    void run() {}
}

class Helper {
    static int helper() { return 1; }
}
EOF
	git -C "$repo" add src/Service.java
	git -C "$repo" commit -qm "add Java source"
}

add_kotlin_source() {
	repo=$1
	mkdir -p "$repo/src"
	cat >"$repo/src/Service.kt" <<'EOF'
class Service {
    fun run() {}
}

object Helper {
    fun helper() = 1
}
EOF
	git -C "$repo" add src/Service.kt
	git -C "$repo" commit -qm "add Kotlin source"
}

add_rust_source() {
	repo=$1
	mkdir -p "$repo/crates/core/src"
	cat >"$repo/crates/core/src/lib.rs" <<'EOF'
pub struct Service;

impl Service {
    pub fn run(&self) {}
}

pub mod helpers {
    pub fn helper() {}
}
EOF
	git -C "$repo" add crates/core/src/lib.rs
	git -C "$repo" commit -qm "add Rust source"
}

echo "scenario: CLI binary is present"
"$THREAD_KEEP" --help >/dev/null

echo "scenario: non-Git repository error"
NON_GIT="$WORKDIR/non-git"
mkdir -p "$NON_GIT"
expect_json_error 3 repository_state "$THREAD_KEEP" --repo "$NON_GIT" --json status

echo "scenario: initialization boundary and local workflow"
REPO="$WORKDIR/main"
init_repo "$REPO"
expect_json_error 4 not_initialized "$THREAD_KEEP" --repo "$REPO" --json update
[ ! -e "$REPO/.git/thread-keep/index.sqlite" ] || fail "update created storage before init"

run "$THREAD_KEEP" --repo "$REPO" --json init
assert_contains "$LAST_STDOUT" '"initialized":true'
assert_file "$REPO/.git/thread-keep/index.sqlite"
run "$THREAD_KEEP" --repo "$REPO" --json init
assert_contains "$LAST_STDOUT" '"initialized":true'

run "$THREAD_KEEP" --repo "$REPO" --json update
assert_contains "$LAST_STDOUT" '"indexed_entities":3'
run "$THREAD_KEEP" --repo "$REPO" --json note add sample.Run --kind intent --body "결제 승인 재시도는 중복 청구를 만들면 안 된다" --origin agent
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
run "$THREAD_KEEP" --repo "$REPO" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":1'
run "$THREAD_KEEP" --repo "$REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
run "$THREAD_KEEP" --repo "$REPO" --json search "중복 청구"
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
run "$THREAD_KEEP" --repo "$REPO" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"pending":true'
assert_contains "$LAST_STDOUT" '"결제 승인 재시도는 중복 청구를 만들면 안 된다"'
run "$THREAD_KEEP" --repo "$REPO" --json diff
assert_contains "$LAST_STDOUT" '"kind":"intent"'

run "$THREAD_KEEP" --repo "$REPO" --json commit -m "document payment retry" --author e2e
assert_contains "$LAST_STDOUT" '"message":"document payment retry"'
[ "$(find "$REPO/.git/thread-keep/objects" -name '*.json' -type f | wc -l | tr -d ' ')" -eq 1 ] || fail "expected one immutable context object"
run "$THREAD_KEEP" --repo "$REPO" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":0'
run "$THREAD_KEEP" --repo "$REPO" --json log
assert_contains "$LAST_STDOUT" '"message":"document payment retry"'

echo "scenario: validation and state-preserving failures"
expect_json_error 2 validation "$THREAD_KEEP" --repo "$REPO" --json commit
expect_json_error 7 entity_not_found "$THREAD_KEEP" --repo "$REPO" --json note add missing.Entity --kind intent --body missing
expect_json_error 5 nothing_to_commit "$THREAD_KEEP" --repo "$REPO" --json commit -m "empty working set"

printf '%s\n' 'func Dirty() {}' >>"$REPO/sample.go"
expect_json_error 3 repository_state "$THREAD_KEEP" --repo "$REPO" --json update
git -C "$REPO" checkout -- sample.go

run "$THREAD_KEEP" --repo "$REPO" --json note add sample.Run --kind warning --body "commit source first"
printf '%s\n' 'func SourceChanged() {}' >>"$REPO/sample.go"
git -C "$REPO" add sample.go
git -C "$REPO" commit -qm "change source"
expect_json_error 5 working_set_dirty "$THREAD_KEEP" --repo "$REPO" --json update
run "$THREAD_KEEP" --repo "$REPO" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":1'

echo "scenario: linked worktree pending-state isolation"
ISOLATION_REPO="$WORKDIR/isolation"
init_repo "$ISOLATION_REPO"
run "$THREAD_KEEP" --repo "$ISOLATION_REPO" init
run "$THREAD_KEEP" --repo "$ISOLATION_REPO" update
run "$THREAD_KEEP" --repo "$ISOLATION_REPO" note add sample.Run --kind intent --body "primary note"
LINKED="$WORKDIR/linked"
git -C "$ISOLATION_REPO" worktree add -q -b linked "$LINKED"
run "$THREAD_KEEP" --repo "$LINKED" update
run "$THREAD_KEEP" --repo "$LINKED" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":0'
run "$THREAD_KEEP" --repo "$LINKED" note add sample.Run --kind decision --body "linked note"
run "$THREAD_KEEP" --repo "$LINKED" commit -m "linked context"
run "$THREAD_KEEP" --repo "$ISOLATION_REPO" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":1'

echo "scenario: dotted and nested paths have distinct entity keys"
PATH_REPO="$WORKDIR/path-identity"
init_repo "$PATH_REPO"
mkdir -p "$PATH_REPO/a.b" "$PATH_REPO/a/b"
printf '%s\n' 'package main' 'func Run() {}' >"$PATH_REPO/a.b/main.go"
printf '%s\n' 'package main' 'func Run() {}' >"$PATH_REPO/a/b/main.go"
git -C "$PATH_REPO" add a.b/main.go a/b/main.go
git -C "$PATH_REPO" commit -qm "add distinct path identities"
run "$THREAD_KEEP" --repo "$PATH_REPO" init
run "$THREAD_KEEP" --repo "$PATH_REPO" update
run "$THREAD_KEEP" --repo "$PATH_REPO" --json context get a.b/main.Run
assert_contains "$LAST_STDOUT" '"key":"a.b/main.Run"'
run "$THREAD_KEEP" --repo "$PATH_REPO" --json context get a/b/main.Run
assert_contains "$LAST_STDOUT" '"key":"a/b/main.Run"'

echo "scenario: installed TypeScript pack indexes mixed repositories"
MIXED_REPO="$WORKDIR/mixed"
init_repo "$MIXED_REPO"
add_typescript_source "$MIXED_REPO"
run "$THREAD_KEEP" --repo "$MIXED_REPO" init
run "$THREAD_KEEP" --repo "$MIXED_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":true'
assert_contains "$LAST_STDOUT" '"language":"typescript","state":"indexed"'
run "$THREAD_KEEP" --repo "$MIXED_REPO" --json search helper
assert_contains "$LAST_STDOUT" '"entity_key":"typescript:web/app.ts#function:helper"'
run "$THREAD_KEEP" --repo "$MIXED_REPO" note add sample.Run --kind intent --body "mixed coverage is canonical"
run "$THREAD_KEEP" --repo "$MIXED_REPO" commit -m "commit mixed context"

echo "scenario: installed JavaScript pack indexes mixed repositories"
JAVASCRIPT_REPO="$WORKDIR/javascript"
init_repo "$JAVASCRIPT_REPO"
add_javascript_source "$JAVASCRIPT_REPO"
run "$THREAD_KEEP" --repo "$JAVASCRIPT_REPO" init
run "$THREAD_KEEP" --repo "$JAVASCRIPT_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":true'
assert_contains "$LAST_STDOUT" '"language":"javascript","state":"indexed"'
run "$THREAD_KEEP" --repo "$JAVASCRIPT_REPO" --json search helper
assert_contains "$LAST_STDOUT" '"entity_key":"javascript:web/app.js#function:helper"'
run "$THREAD_KEEP" --repo "$JAVASCRIPT_REPO" note add sample.Run --kind intent --body "JavaScript coverage is canonical"
run "$THREAD_KEEP" --repo "$JAVASCRIPT_REPO" commit -m "commit JavaScript context"

echo "scenario: installed Python pack indexes mixed repositories"
PYTHON_REPO="$WORKDIR/python"
init_repo "$PYTHON_REPO"
add_python_source "$PYTHON_REPO"
run "$THREAD_KEEP" --repo "$PYTHON_REPO" init
run "$THREAD_KEEP" --repo "$PYTHON_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":true'
assert_contains "$LAST_STDOUT" '"language":"python","state":"indexed"'
run "$THREAD_KEEP" --repo "$PYTHON_REPO" --json search helper
assert_contains "$LAST_STDOUT" '"entity_key":"python:services/app.py#function:helper"'
run "$THREAD_KEEP" --repo "$PYTHON_REPO" note add sample.Run --kind intent --body "Python coverage is canonical"
run "$THREAD_KEEP" --repo "$PYTHON_REPO" commit -m "commit Python context"

echo "scenario: installed Java pack indexes mixed repositories"
JAVA_REPO="$WORKDIR/java"
init_repo "$JAVA_REPO"
add_java_source "$JAVA_REPO"
run "$THREAD_KEEP" --repo "$JAVA_REPO" init
run "$THREAD_KEEP" --repo "$JAVA_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":true'
assert_contains "$LAST_STDOUT" '"language":"java","state":"indexed"'
run "$THREAD_KEEP" --repo "$JAVA_REPO" --json search helper
assert_contains "$LAST_STDOUT" '"entity_key":"java:src/Service.java#method:Helper.helper"'
run "$THREAD_KEEP" --repo "$JAVA_REPO" note add sample.Run --kind intent --body "Java coverage is canonical"
run "$THREAD_KEEP" --repo "$JAVA_REPO" commit -m "commit Java context"

echo "scenario: installed Kotlin pack indexes mixed repositories"
KOTLIN_REPO="$WORKDIR/kotlin"
init_repo "$KOTLIN_REPO"
add_kotlin_source "$KOTLIN_REPO"
run "$THREAD_KEEP" --repo "$KOTLIN_REPO" init
run "$THREAD_KEEP" --repo "$KOTLIN_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":true'
assert_contains "$LAST_STDOUT" '"language":"kotlin","state":"indexed"'
run "$THREAD_KEEP" --repo "$KOTLIN_REPO" --json search helper
assert_contains "$LAST_STDOUT" '"entity_key":"kotlin:src/Service.kt#method:Helper.helper"'
run "$THREAD_KEEP" --repo "$KOTLIN_REPO" note add sample.Run --kind intent --body "Kotlin coverage is canonical"
run "$THREAD_KEEP" --repo "$KOTLIN_REPO" commit -m "commit Kotlin context"

echo "scenario: installed Rust pack indexes mixed repositories"
RUST_REPO="$WORKDIR/rust"
init_repo "$RUST_REPO"
add_rust_source "$RUST_REPO"
run "$THREAD_KEEP" --repo "$RUST_REPO" init
run "$THREAD_KEEP" --repo "$RUST_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":true'
assert_contains "$LAST_STDOUT" '"language":"rust","state":"indexed"'
run "$THREAD_KEEP" --repo "$RUST_REPO" --json search helper
assert_contains "$LAST_STDOUT" '"entity_key":"rust:crates/core/src/lib.rs#function:helpers.helper"'
run "$THREAD_KEEP" --repo "$RUST_REPO" note add sample.Run --kind intent --body "Rust coverage is canonical"
run "$THREAD_KEEP" --repo "$RUST_REPO" commit -m "commit Rust context"

echo "scenario: missing TypeScript pack keeps Go queryable but blocks strict update and commit"
MISSING_REPO="$WORKDIR/missing-pack"
EMPTY_CONFIG="$WORKDIR/empty-config"
init_repo "$MISSING_REPO"
add_typescript_source "$MISSING_REPO"
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_REPO" init
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":false'
assert_contains "$LAST_STDOUT" '"language":"typescript","state":"missing_pack"'
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_REPO" --json update --require-complete
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_REPO" note add sample.Run --kind intent --body "wait for pack"
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_REPO" --json commit -m "must not commit"

echo "scenario: missing JavaScript pack keeps Go queryable but blocks strict update and commit"
MISSING_JAVASCRIPT_REPO="$WORKDIR/missing-javascript-pack"
init_repo "$MISSING_JAVASCRIPT_REPO"
add_javascript_source "$MISSING_JAVASCRIPT_REPO"
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVASCRIPT_REPO" init
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVASCRIPT_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":false'
assert_contains "$LAST_STDOUT" '"language":"javascript","state":"missing_pack"'
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVASCRIPT_REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVASCRIPT_REPO" --json update --require-complete
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVASCRIPT_REPO" note add sample.Run --kind intent --body "wait for JavaScript pack"
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVASCRIPT_REPO" --json commit -m "must not commit"

echo "scenario: missing Python pack keeps Go queryable but blocks strict update and commit"
MISSING_PYTHON_REPO="$WORKDIR/missing-python-pack"
init_repo "$MISSING_PYTHON_REPO"
add_python_source "$MISSING_PYTHON_REPO"
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_PYTHON_REPO" init
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_PYTHON_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":false'
assert_contains "$LAST_STDOUT" '"language":"python","state":"missing_pack"'
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_PYTHON_REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_PYTHON_REPO" --json update --require-complete
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_PYTHON_REPO" note add sample.Run --kind intent --body "wait for Python pack"
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_PYTHON_REPO" --json commit -m "must not commit"

echo "scenario: missing Java pack keeps Go queryable but blocks strict update and commit"
MISSING_JAVA_REPO="$WORKDIR/missing-java-pack"
init_repo "$MISSING_JAVA_REPO"
add_java_source "$MISSING_JAVA_REPO"
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVA_REPO" init
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVA_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":false'
assert_contains "$LAST_STDOUT" '"language":"java","state":"missing_pack"'
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVA_REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVA_REPO" --json update --require-complete
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVA_REPO" note add sample.Run --kind intent --body "wait for Java pack"
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_JAVA_REPO" --json commit -m "must not commit"

echo "scenario: missing Kotlin pack keeps Go queryable but blocks strict update and commit"
MISSING_KOTLIN_REPO="$WORKDIR/missing-kotlin-pack"
init_repo "$MISSING_KOTLIN_REPO"
add_kotlin_source "$MISSING_KOTLIN_REPO"
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_KOTLIN_REPO" init
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_KOTLIN_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":false'
assert_contains "$LAST_STDOUT" '"language":"kotlin","state":"missing_pack"'
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_KOTLIN_REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_KOTLIN_REPO" --json update --require-complete
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_KOTLIN_REPO" note add sample.Run --kind intent --body "wait for Kotlin pack"
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_KOTLIN_REPO" --json commit -m "must not commit"

echo "scenario: missing Rust pack keeps Go queryable but blocks strict update and commit"
MISSING_RUST_REPO="$WORKDIR/missing-rust-pack"
init_repo "$MISSING_RUST_REPO"
add_rust_source "$MISSING_RUST_REPO"
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_RUST_REPO" init
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_RUST_REPO" --json update
assert_contains "$LAST_STDOUT" '"coverage_complete":false'
assert_contains "$LAST_STDOUT" '"language":"rust","state":"missing_pack"'
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_RUST_REPO" --json search Run
assert_contains "$LAST_STDOUT" '"entity_key":"sample.Run"'
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_RUST_REPO" --json update --require-complete
run env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_RUST_REPO" note add sample.Run --kind intent --body "wait for Rust pack"
expect_json_error 5 coverage_incomplete env XDG_CONFIG_HOME="$EMPTY_CONFIG" "$THREAD_KEEP" --repo "$MISSING_RUST_REPO" --json commit -m "must not commit"

echo "scenario: HTTP context remote enforces GitHub permissions for push and pull"
[ -x "$THREAD_KEEP_SERVER" ] || fail "thread-keep-server binary is not executable: $THREAD_KEEP_SERVER"
[ -x "$THREAD_KEEP_E2E_FAKEGITHUB" ] || fail "fake github binary is not executable: $THREAD_KEEP_E2E_FAKEGITHUB"

FAKE_GITHUB_LOG="$WORKDIR/fake-github.log"
"$THREAD_KEEP_E2E_FAKEGITHUB" --listen 127.0.0.1:18081 >"$FAKE_GITHUB_LOG" 2>&1 &
FAKE_GITHUB_PID=$!
wait_for_listen "$FAKE_GITHUB_LOG"

SERVER_STORAGE="$WORKDIR/server-storage"
mkdir -p "$SERVER_STORAGE"
cat >"$WORKDIR/server-config.json" <<'EOF'
{
  "github_api_base_url": "http://127.0.0.1:18081",
  "repositories": {
    "repo-e2e": { "github_owner": "acme", "github_repo": "thread-keep" },
    "repo-merge": { "github_owner": "acme", "github_repo": "thread-keep" }
  }
}
EOF
SERVER_LOG="$WORKDIR/server.log"
"$THREAD_KEEP_SERVER" --listen 127.0.0.1:18320 --storage "$SERVER_STORAGE" --config "$WORKDIR/server-config.json" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
wait_for_listen "$SERVER_LOG"

HTTP_PRIMARY="$WORKDIR/http-primary"
init_repo "$HTTP_PRIMARY"
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json init
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json update
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json note add sample.Run --kind intent --body "shared over the http context remote"
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json note add sample.Run --kind warning --body "retry must not create duplicate work"
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json note add sample.Worker --kind decision --body "worker stays single-threaded by design"
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json note add sample.Worker --kind constraint --body "worker state must fit in memory"
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json note add sample.Worker.Execute --kind example --body "call Execute exactly once per job"
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":5'
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json commit -m "share context over http" --author e2e
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json remote add origin http://127.0.0.1:18320/v1/repositories/repo-e2e

unset THREAD_KEEP_REMOTE_TOKEN || true
expect_json_error 8 auth "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json remote push origin
export THREAD_KEEP_REMOTE_TOKEN=reader-token
expect_json_error 8 auth "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json remote push origin
export THREAD_KEEP_REMOTE_TOKEN=writer-token
run "$THREAD_KEEP" --repo "$HTTP_PRIMARY" --json remote push origin
assert_contains "$LAST_STDOUT" '"outcome":"pushed"'
assert_file "$SERVER_STORAGE/refs.db"
[ "$(find "$SERVER_STORAGE/repo-e2e/objects" -name '*.json' -type f | wc -l | tr -d ' ')" -eq 1 ] || fail "expected one published object on the http remote"

HTTP_SECONDARY="$WORKDIR/http-secondary"
git clone -q --no-local "$HTTP_PRIMARY" "$HTTP_SECONDARY"
git -C "$HTTP_SECONDARY" config user.name "Thread Keep E2E"
git -C "$HTTP_SECONDARY" config user.email "thread-keep-e2e@example.test"
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json init
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json update
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json remote add origin http://127.0.0.1:18320/v1/repositories/repo-e2e
export THREAD_KEEP_REMOTE_TOKEN=reader-token
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json remote pull origin
assert_contains "$LAST_STDOUT" '"outcome":"pulled"'
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"kind":"intent"'
assert_contains "$LAST_STDOUT" '"shared over the http context remote"'
assert_contains "$LAST_STDOUT" '"kind":"warning"'
assert_contains "$LAST_STDOUT" '"retry must not create duplicate work"'
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json context get sample.Worker
assert_contains "$LAST_STDOUT" '"kind":"decision"'
assert_contains "$LAST_STDOUT" '"worker stays single-threaded by design"'
assert_contains "$LAST_STDOUT" '"kind":"constraint"'
assert_contains "$LAST_STDOUT" '"worker state must fit in memory"'
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json context get sample.Worker.Execute
assert_contains "$LAST_STDOUT" '"kind":"example"'
assert_contains "$LAST_STDOUT" '"call Execute exactly once per job"'
run "$THREAD_KEEP" --repo "$HTTP_SECONDARY" --json remote pull origin
assert_contains "$LAST_STDOUT" '"outcome":"up_to_date"'
unset THREAD_KEEP_REMOTE_TOKEN || true

echo "scenario: two-node context-remote cluster replicates objects and survives node loss"
CLUSTER_DIR="$WORKDIR/cluster"
mkdir -p "$CLUSTER_DIR/storage-a" "$CLUSTER_DIR/storage-b"
write_cluster_config() {
	target=$1
	node_id=$2
	advertise=$3
	cat >"$target" <<EOF
{
  "github_api_base_url": "http://127.0.0.1:18081",
  "repositories": {
    "repo-cluster": { "github_owner": "acme", "github_repo": "thread-keep" }
  },
  "cluster": {
    "node_id": "$node_id",
    "advertise_url": "$advertise",
    "replication_factor": 2,
    "heartbeat_seconds": 1,
    "ttl_seconds": 5,
    "anti_entropy_seconds": 60
  }
}
EOF
}
write_cluster_config "$CLUSTER_DIR/config-a.json" node-a http://127.0.0.1:18330
write_cluster_config "$CLUSTER_DIR/config-b.json" node-b http://127.0.0.1:18331
export THREAD_KEEP_CLUSTER_SECRET=e2e-cluster-secret

CLUSTER_A_LOG="$CLUSTER_DIR/node-a.log"
"$THREAD_KEEP_SERVER" --listen 127.0.0.1:18330 --storage "$CLUSTER_DIR/storage-a" --config "$CLUSTER_DIR/config-a.json" --db-dsn "$CLUSTER_DIR/refs.db" >"$CLUSTER_A_LOG" 2>&1 &
CLUSTER_A_PID=$!
wait_for_listen "$CLUSTER_A_LOG"
CLUSTER_B_LOG="$CLUSTER_DIR/node-b.log"
"$THREAD_KEEP_SERVER" --listen 127.0.0.1:18331 --storage "$CLUSTER_DIR/storage-b" --config "$CLUSTER_DIR/config-b.json" --db-dsn "$CLUSTER_DIR/refs.db" >"$CLUSTER_B_LOG" 2>&1 &
CLUSTER_B_PID=$!
wait_for_listen "$CLUSTER_B_LOG"

CLUSTER_PRIMARY="$WORKDIR/cluster-primary"
init_repo "$CLUSTER_PRIMARY"
run "$THREAD_KEEP" --repo "$CLUSTER_PRIMARY" --json init
run "$THREAD_KEEP" --repo "$CLUSTER_PRIMARY" --json update
run "$THREAD_KEEP" --repo "$CLUSTER_PRIMARY" --json note add sample.Run --kind intent --body "handed over through the cluster"
run "$THREAD_KEEP" --repo "$CLUSTER_PRIMARY" --json commit -m "cluster handover" --author e2e
run "$THREAD_KEEP" --repo "$CLUSTER_PRIMARY" --json remote add origin http://127.0.0.1:18330/v1/repositories/repo-cluster
export THREAD_KEEP_REMOTE_TOKEN=writer-token
run "$THREAD_KEEP" --repo "$CLUSTER_PRIMARY" --json remote push origin
assert_contains "$LAST_STDOUT" '"outcome":"pushed"'
[ "$(find "$CLUSTER_DIR/storage-a/repo-cluster/objects" -name '*.json' -type f | wc -l | tr -d ' ')" -eq 1 ] || fail "expected the pushed object on node-a"
[ "$(find "$CLUSTER_DIR/storage-b/repo-cluster/objects" -name '*.json' -type f | wc -l | tr -d ' ')" -eq 1 ] || fail "expected the write-through replica on node-b"

kill "$CLUSTER_A_PID" 2>/dev/null || true
attempts=0
until grep -q "shutting down" "$CLUSTER_A_LOG" 2>/dev/null; do
	attempts=$((attempts + 1))
	[ "$attempts" -le 100 ] || fail "node-a did not shut down gracefully: $(cat "$CLUSTER_A_LOG")"
	sleep 0.1
done
CLUSTER_A_PID=""

CLUSTER_SECONDARY="$WORKDIR/cluster-secondary"
git clone -q --no-local "$CLUSTER_PRIMARY" "$CLUSTER_SECONDARY"
git -C "$CLUSTER_SECONDARY" config user.name "Thread Keep E2E"
git -C "$CLUSTER_SECONDARY" config user.email "thread-keep-e2e@example.test"
run "$THREAD_KEEP" --repo "$CLUSTER_SECONDARY" --json init
run "$THREAD_KEEP" --repo "$CLUSTER_SECONDARY" --json update
run "$THREAD_KEEP" --repo "$CLUSTER_SECONDARY" --json remote add origin http://127.0.0.1:18331/v1/repositories/repo-cluster
export THREAD_KEEP_REMOTE_TOKEN=reader-token
run "$THREAD_KEEP" --repo "$CLUSTER_SECONDARY" --json remote pull origin
assert_contains "$LAST_STDOUT" '"outcome":"pulled"'
run "$THREAD_KEEP" --repo "$CLUSTER_SECONDARY" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"handed over through the cluster"'

"$THREAD_KEEP_SERVER" --gc --gc-grace 0s --storage "$CLUSTER_DIR/storage-b" --config "$CLUSTER_DIR/config-b.json" --db-dsn "$CLUSTER_DIR/refs.db" >"$CLUSTER_DIR/gc.json" || fail "gc pass failed"
assert_contains "$CLUSTER_DIR/gc.json" '"deleted":0'
assert_contains "$CLUSTER_DIR/gc.json" '"kept":1'
assert_contains "$CLUSTER_DIR/gc.json" '"packed":1'
[ "$(find "$CLUSTER_DIR/storage-b/repo-cluster/packs" -name '*.pack' -type f | wc -l | tr -d ' ')" -eq 1 ] || fail "expected the reachable object repacked on node-b"
export THREAD_KEEP_REMOTE_TOKEN=reader-token
run "$THREAD_KEEP" --repo "$CLUSTER_SECONDARY" --json remote pull origin
assert_contains "$LAST_STDOUT" '"outcome":"up_to_date"'
unset THREAD_KEEP_REMOTE_TOKEN || true
unset THREAD_KEEP_CLUSTER_SECRET || true

echo "scenario: MCP server drafts an agent-origin pending note over stdio"
THREAD_KEEP_MCP=${THREAD_KEEP_MCP:-/usr/local/bin/thread-keep-mcp}
[ -x "$THREAD_KEEP_MCP" ] || fail "thread-keep-mcp binary is not executable: $THREAD_KEEP_MCP"
[ -x "$THREAD_KEEP_E2E_MCPCLIENT" ] || fail "MCP E2E client is not executable: $THREAD_KEEP_E2E_MCPCLIENT"
MCP_REPO="$WORKDIR/mcp-repo"
init_repo "$MCP_REPO"
run "$THREAD_KEEP" --repo "$MCP_REPO" --json init
run "$THREAD_KEEP" --repo "$MCP_REPO" --json update
MCP_OUT="$WORKDIR/mcp.out"
"$THREAD_KEEP_E2E_MCPCLIENT" --server "$THREAD_KEEP_MCP" --repo "$MCP_REPO" >"$MCP_OUT" 2>/dev/null || fail "mcp server failed"
assert_contains "$MCP_OUT" '"thread-keep"'
assert_contains "$MCP_OUT" '"note_add"'
assert_contains "$MCP_OUT" '\"origin\":\"agent\"'
run "$THREAD_KEEP" --repo "$MCP_REPO" --json status
assert_contains "$LAST_STDOUT" '"pending_notes":1'

extract_data_id() {
	sed -n 's/.*"data":{"id":"\([0-9a-f]*\)".*/\1/p' "$1" | head -1
}

echo "scenario: stale context is blocked until an explicit review"
STALE_REPO="$WORKDIR/stale-repo"
init_repo "$STALE_REPO"
run "$THREAD_KEEP" --repo "$STALE_REPO" --json init
run "$THREAD_KEEP" --repo "$STALE_REPO" --json update
run "$THREAD_KEEP" --repo "$STALE_REPO" --json note add sample.Run --kind intent --body "stale lifecycle contract"
STALE_NOTE_ID=$(extract_data_id "$LAST_STDOUT")
[ -n "$STALE_NOTE_ID" ] || fail "could not extract the note id"
run "$THREAD_KEEP" --repo "$STALE_REPO" --json commit -m "note before change" --author e2e
run "$THREAD_KEEP" --repo "$STALE_REPO" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"stale lifecycle contract"'

printf 'package sample\n\ntype Worker struct{}\n\nfunc Run() { _ = 1 }\n\nfunc (Worker) Execute() {}\n' >"$STALE_REPO/sample.go"
git -C "$STALE_REPO" add sample.go
git -C "$STALE_REPO" commit -qm "change Run structurally"
run "$THREAD_KEEP" --repo "$STALE_REPO" --json update

run "$THREAD_KEEP" --repo "$STALE_REPO" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"notes":null'
run "$THREAD_KEEP" --repo "$STALE_REPO" --json search "stale lifecycle contract"
assert_contains "$LAST_STDOUT" '"data":[]'
run "$THREAD_KEEP" --repo "$STALE_REPO" --json diff
assert_contains "$LAST_STDOUT" '"binding_state":"needs_review"'
assert_contains "$LAST_STDOUT" '"review_reason":"structural_change"'

run "$THREAD_KEEP" --repo "$STALE_REPO" --json note review "$STALE_NOTE_ID" --entity sample.Run
run "$THREAD_KEEP" --repo "$STALE_REPO" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"stale lifecycle contract"'
run "$THREAD_KEEP" --repo "$STALE_REPO" --json commit -m "reviewed binding" --author e2e

echo "scenario: divergent context resolves through an explicit semantic merge over the HTTP remote"
MERGE_REMOTE=http://127.0.0.1:18320/v1/repositories/repo-merge
MERGE_PRIMARY="$WORKDIR/merge-primary"
MERGE_SECONDARY="$WORKDIR/merge-secondary"
init_repo "$MERGE_PRIMARY"
git clone -q --no-local "$MERGE_PRIMARY" "$MERGE_SECONDARY"
git -C "$MERGE_SECONDARY" config user.name "Thread Keep E2E"
git -C "$MERGE_SECONDARY" config user.email "thread-keep-e2e@example.test"
export THREAD_KEEP_REMOTE_TOKEN=writer-token

run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json init
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json update
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json note add sample.Run --kind decision --body "shared base decision"
MERGE_NOTE_ID=$(extract_data_id "$LAST_STDOUT")
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json commit -m "shared base" --author e2e
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json remote add origin "$MERGE_REMOTE"
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json remote push origin

run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json init
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json update
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json remote add origin "$MERGE_REMOTE"
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json remote pull origin

run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json note revise "$MERGE_NOTE_ID" --body "primary revision"
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json commit -m "primary revision" --author e2e
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json remote push origin

run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json note revise "$MERGE_NOTE_ID" --body "secondary revision"
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json commit -m "secondary revision" --author e2e
MERGE_LOCAL_TIP=$(extract_data_id "$LAST_STDOUT")
expect_json_error 6 remote_conflict "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json remote push origin

run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json remote fetch origin
MERGE_REMOTE_TIP=$(sed -n 's/.*"tracking_tip":"\([0-9a-f]*\)".*/\1/p' "$LAST_STDOUT" | head -1)
[ -n "$MERGE_LOCAL_TIP" ] && [ -n "$MERGE_REMOTE_TIP" ] || fail "could not extract merge snapshot ids"

run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json context merge start "$MERGE_LOCAL_TIP" "$MERGE_REMOTE_TIP" -m "merge review" --author reviewer
MERGE_SESSION_ID=$(extract_data_id "$LAST_STDOUT")
MERGE_CONFLICT_ID=$(sed -n 's/.*"conflicts":\[{"id":"\([0-9a-f]*\)".*/\1/p' "$LAST_STDOUT" | head -1)
[ -n "$MERGE_SESSION_ID" ] && [ -n "$MERGE_CONFLICT_ID" ] || fail "merge session did not report a competing-revision conflict"

run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json context merge resolve "$MERGE_SESSION_ID" "$MERGE_CONFLICT_ID" --use remote
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json context merge commit "$MERGE_SESSION_ID"
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json remote push origin
assert_contains "$LAST_STDOUT" '"outcome":"pushed"'
run "$THREAD_KEEP" --repo "$MERGE_PRIMARY" --json remote pull origin
assert_contains "$LAST_STDOUT" '"outcome":"pulled"'
run "$THREAD_KEEP" --repo "$MERGE_SECONDARY" --json context get sample.Run
assert_contains "$LAST_STDOUT" '"primary revision"'
unset THREAD_KEEP_REMOTE_TOKEN || true

echo "all Docker E2E scenarios passed"
