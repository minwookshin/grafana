# Generic Create-or-Replace via Sentinel ResourceVersion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `Create` on any app-sdk resource behave as an upsert — replacing an existing object of the same name instead of failing with `AlreadyExists` — when the caller sets `metadata.resourceVersion` to a reserved sentinel value, implemented once in the shared `apistore` layer.

**Architecture:** `apistore.Storage.Create` (`pkg/storage/unified/apistore/store.go`) detects the sentinel resourceVersion before doing anything else, clears it, and branches into a new `createOrReplace` method: `Get` the target first; if missing, fall through to a normal create (now with a legitimately empty RV); if found, replace it via the existing `GuaranteedUpdate` — which already enforces provisioning-lock checks (`checkManagerPropertiesOnUpdateSpec`) and independently re-authorizes `VerbUpdate` server-side (`server.update`'s `s.access.Check`), so no new authorization code is needed. Every other resourceVersion value (empty or any other non-empty string) must produce exactly the behavior that exists today.

**Tech Stack:** Go, `k8s.io/apiserver/pkg/storage.Interface`, Grafana's unified storage (`pkg/storage/unified/apistore`, `pkg/storage/unified/resource`), `testify`, the existing BadgerDB-backed in-process test harness in `pkg/storage/unified/apistore/watcher_test.go`, and the `pkg/tests/apis` k8s integration-test harness for the generalization proof.

**Spec:** `docs/superpowers/specs/2026-10-01-generic-create-or-replace-resourceversion-design.md`

## Global Constraints

- The sentinel value is `"-1"` (checked for collisions against existing apistore/apimachinery code — none found).
- Every resourceVersion value other than the sentinel (empty, or any other non-empty string) must produce byte-for-byte the same behavior as before this change. This is the plan's most important property — every task that touches `Create` re-runs the pre-existing `TestCreate`/`TestCreateWithKeyExist` conformance tests and treats any difference as a bug in the new code, never as a test to adjust.
- No feature toggle. The sentinel is new and today produces a hard validation error for every caller, so recognizing it cannot change any existing caller's behavior.
- Out of scope, do not touch: `terraform-provider-grafana` (separate repo, separate follow-on), quota admission code (runs upstream of apistore, per-resource follow-up), the `ManagerKindTerraform`/`ManagerKindKubectl` `AllowsEdits` no-op at `pkg/storage/unified/apistore/managed.go:205-210` (pre-existing gap, not introduced or worsened here).

## Review Focus

- **A `Get` inside `createOrReplace` fails with something other than NotFound** (e.g. `Forbidden` on an object in a namespace the caller can't read) — must propagate unchanged, not get masked behind a synthetic conflict or swallowed. Covered in Task 2.
- **The target object is Repo-managed (Git Sync)** — a non-provisioning caller's replace must be rejected exactly like a normal Update to that object would be, not silently allowed through because it arrived via Create. Covered in Task 3.
- **The caller has create rights but not update rights on the specific existing object** — must surface a real `Forbidden`, not a misleading success or a 409. This is the core authorization property the whole design rests on. Covered in Task 4.
- **Every non-sentinel resourceVersion value** (empty, and some other arbitrary non-empty string) must still produce today's exact behavior — a regression here would be a silent, cross-cutting behavior change for every app-sdk resource, not a localized bug. Covered in Task 2.
- **The mechanism actually works for a resource type other than the one it was designed against** — since the whole point is "generic, not dashboard-specific," a passing design on paper isn't evidence; this needs to be demonstrated on a second resource type end-to-end. Covered in Task 5 (folders).

---

## File Structure

- **Modify** `pkg/storage/unified/apistore/store.go` — add the sentinel constant, the branch in `Create`, and the new `createOrReplace` method.
- **Modify** `pkg/storage/unified/apistore/store_test.go` — new test functions for the happy path, edge cases, and the provisioning-lock case.
- **Modify** `pkg/storage/unified/apistore/watcher_test.go` — add a `setupOption` to inject a custom `claims.AccessClient` into the in-process test server, needed only by Task 4's RBAC-denial test.
- **Create** `pkg/tests/apis/folder/overwrite_test.go` — integration test proving the mechanism works for folders, not just the resource type it was designed against.

---

### Task 1: Sentinel detection + happy path (create-when-missing, replace-when-found)

**Files:**
- Modify: `pkg/storage/unified/apistore/store.go:65-72` (new const), `store.go:319-388` (`Create`, add the branch)
- Test: `pkg/storage/unified/apistore/store_test.go`

**Interfaces:**
- Produces: `apistore.OverwriteOnCreateResourceVersion` (exported `string` constant, value `"-1"`) and `(s *Storage) createOrReplace(ctx context.Context, key string, obj runtime.Object, out runtime.Object, ttl uint64) error` (unexported method) — Tasks 2-4 add more tests against this same method; Task 5 exercises it indirectly through the real apiserver.

- [ ] **Step 1: Confirm the regression baseline is green before touching anything**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreate$|TestCreateWithKeyExist$' -v`

Expected: `PASS`. This is the anchor you'll re-run after every later step in this plan — if it ever differs from this baseline, the new code is wrong, not the test.

- [ ] **Step 2: Write the failing tests**

Add to `pkg/storage/unified/apistore/store_test.go` (the file already imports `example`, `metav1`, `require`, `apistore`, `testutil`, etc. — reuse those, don't re-add):

```go
func TestCreateOrReplaceCreatesWhenMissing(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t)
	defer destroyFunc()
	require.NoError(t, err)

	key := "/pods/test-ns/overwrite-missing"
	obj := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "overwrite-missing",
			Namespace:       "test-ns",
			ResourceVersion: apistore.OverwriteOnCreateResourceVersion,
		},
	}

	out := &example.Pod{}
	err = store.Create(ctx, key, obj, out, 0)
	require.NoError(t, err)
	require.NotEmpty(t, out.ResourceVersion, "a real resourceVersion must come back from a genuine create")
}

func TestCreateOrReplaceReplacesWhenFound(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t)
	defer destroyFunc()
	require.NoError(t, err)

	key := "/pods/test-ns/overwrite-existing"
	first := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "overwrite-existing", Namespace: "test-ns"},
		Spec:       example.PodSpec{NodeName: "first-node"},
	}
	firstOut := &example.Pod{}
	require.NoError(t, store.Create(ctx, key, first, firstOut, 0))

	second := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "overwrite-existing",
			Namespace:       "test-ns",
			ResourceVersion: apistore.OverwriteOnCreateResourceVersion,
		},
		Spec: example.PodSpec{NodeName: "second-node"},
	}
	secondOut := &example.Pod{}
	err = store.Create(ctx, key, second, secondOut, 0)
	require.NoError(t, err, "expected the sentinel to trigger a replace, not AlreadyExists")
	require.Equal(t, "second-node", secondOut.Spec.NodeName)
	require.Equal(t, firstOut.UID, secondOut.UID, "replace must preserve the original object's identity, proving this went through Update, not a second Create")
	require.NotEqual(t, firstOut.ResourceVersion, secondOut.ResourceVersion, "a real write must bump the resourceVersion")
}
```

- [ ] **Step 3: Run the new tests, confirm they fail**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreateOrReplace' -v`

Expected: both fail. `TestCreateOrReplaceCreatesWhenMissing` fails because today's `Create` rejects any non-empty resourceVersion outright (`storage.ErrResourceVersionSetOnCreate`, from `prepare.go:158-159`) — the sentinel isn't special yet. `TestCreateOrReplaceReplacesWhenFound` fails the same way on its second `Create` call.

- [ ] **Step 4: Implement the sentinel branch and `createOrReplace`**

In `pkg/storage/unified/apistore/store.go`, after the existing `const` block (currently lines 65-72, the `DeprecatedIDState` enum), add a separate constant:

```go
// OverwriteOnCreateResourceVersion is a reserved sentinel value for metadata.resourceVersion
// on a Create request. A client that sets it is asking Create to behave as an upsert: if an
// object of the same name already exists, replace it in full (not merged) instead of failing
// with AlreadyExists. The RBAC (VerbUpdate) and provisioning-lock checks a normal Update
// already enforces still apply, via GuaranteedUpdate below -- there is nothing new to bypass.
const OverwriteOnCreateResourceVersion = "-1"
```

In `Create` (currently starting at line 319), insert the branch right after the existing `meta, err := utils.MetaAccessor(obj)` block and before the namespace check:

```go
func (s *Storage) Create(ctx context.Context, key string, obj runtime.Object, out runtime.Object, ttl uint64) error {
	ctx, span := tracer.Start(ctx, "apistore.Storage.Create")
	defer span.End()

	rkey, err := s.getKey(key)
	if err != nil {
		return err
	}

	meta, err := utils.MetaAccessor(obj)
	if err != nil {
		return err
	}

	if meta.GetResourceVersion() == OverwriteOnCreateResourceVersion {
		meta.SetResourceVersion("")
		return s.createOrReplace(ctx, key, obj, out, ttl)
	}

	// Make sure we are looking at the correct namespace
	if meta.GetNamespace() != rkey.Namespace {
```

(everything from `// Make sure we are looking at the correct namespace` through the end of the existing function body is unchanged — do not touch it.)

Add the new method immediately after `Create` (i.e. right after its closing brace, before `Delete`):

```go
// createOrReplace implements the upsert half of OverwriteOnCreateResourceVersion: if an
// object of this name already exists, replace it via GuaranteedUpdate (full content, no
// resourceVersion precondition) instead of letting Create fail with AlreadyExists.
func (s *Storage) createOrReplace(ctx context.Context, key string, obj runtime.Object, out runtime.Object, ttl uint64) error {
	existing := s.newFunc()
	err := s.Get(ctx, key, storage.GetOptions{}, existing)
	if storage.IsNotFound(err) {
		return s.Create(ctx, key, obj, out, ttl)
	}
	if err != nil {
		return err
	}

	tryUpdate := func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
		return obj, nil, nil
	}
	return s.GuaranteedUpdate(ctx, key, out, false, nil, tryUpdate, nil)
}
```

- [ ] **Step 5: Run the new tests, confirm they pass**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreateOrReplace' -v`

Expected: `PASS` for both.

- [ ] **Step 6: Re-run the regression baseline from Step 1**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreate$|TestCreateWithKeyExist$' -v`

Expected: `PASS`, identical to Step 1. If either fails now, the new branch in `Create` is catching cases it shouldn't — stop and fix `Create`, do not touch the conformance tests.

- [ ] **Step 7: Commit**

```bash
git add pkg/storage/unified/apistore/store.go pkg/storage/unified/apistore/store_test.go
git commit -m "apistore: add create-or-replace via sentinel resourceVersion"
```

---

### Task 2: Edge cases — Get errors propagate unchanged, non-sentinel values stay byte-for-byte unchanged

**Files:**
- Test: `pkg/storage/unified/apistore/store_test.go`
- Modify (only if Step 3 finds a real gap — see below): `pkg/storage/unified/apistore/store.go`

**Interfaces:**
- Consumes: `apistore.OverwriteOnCreateResourceVersion`, `createOrReplace` (Task 1).

- [ ] **Step 1: Write the failing test for non-sentinel values staying unchanged**

Add to `store_test.go`:

```go
func TestCreateNonSentinelResourceVersionsUnchanged(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t)
	defer destroyFunc()
	require.NoError(t, err)

	t.Run("empty RV creates normally", func(t *testing.T) {
		obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "empty-rv", Namespace: "test-ns"}}
		out := &example.Pod{}
		err := store.Create(ctx, "/pods/test-ns/empty-rv", obj, out, 0)
		require.NoError(t, err)
	})

	t.Run("arbitrary non-empty, non-sentinel RV is rejected exactly as before", func(t *testing.T) {
		obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "bogus-rv", Namespace: "test-ns", ResourceVersion: "12345",
		}}
		out := &example.Pod{}
		err := store.Create(ctx, "/pods/test-ns/bogus-rv", obj, out, 0)
		require.ErrorIs(t, err, storage.ErrResourceVersionSetOnCreate)
	})
}
```

This test should already pass today (before Task 1's change) and must still pass after it — it's pinning behavior, not driving new code. Treat a failure here as a sign Task 1's branch condition is too broad (e.g. matching on something other than an exact string comparison against the sentinel).

- [ ] **Step 2: Write the failing test for a Get error other than NotFound propagating unchanged**

`createOrReplace`'s `Get` call can't easily be made to fail with `Forbidden` through the public `Storage` API alone (the in-process test server defaults to allow-everything — see Task 4 for why). Instead, prove the propagation contract at the unit level directly: a `Get` failing for a reason that reaches `createOrReplace` must not be swallowed or rewritten. Add this test, which exercises the real failure mode available at this layer — a namespace mismatch between the key and the object, which `Create` itself already rejects as `BadRequest` before `createOrReplace` is reached for the *original* object, but which also applies if `createOrReplace`'s `Get` targets a different namespace than the stored object's namespace (an object simply not existing in the given key's namespace manifests as NotFound and is covered by Task 1; this test instead proves that a `Get` call with a key the backing store actually errors on — e.g. one with invalid characters the backend rejects — surfaces that error untouched):

```go
func TestCreateOrReplaceGetErrorPropagatesUnchanged(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t)
	defer destroyFunc()
	require.NoError(t, err)

	// An empty name produces a key the backend itself rejects when read, independent of
	// whether anything exists there - exercising the "Get fails for a reason other than
	// NotFound" branch without needing a second, more invasive test harness.
	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "", Namespace: "test-ns", ResourceVersion: apistore.OverwriteOnCreateResourceVersion,
	}}
	out := &example.Pod{}
	err = store.Create(ctx, "/pods/test-ns/", obj, out, 0)
	require.Error(t, err)
	require.False(t, storage.IsNotFound(err), "expected a real error to propagate, not be treated as NotFound and silently proceed to create")
}
```

- [ ] **Step 3: Run both new tests**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreateNonSentinelResourceVersionsUnchanged|TestCreateOrReplaceGetErrorPropagatesUnchanged' -v`

Expected: `PASS` for both, with no code changes needed — Task 1's implementation already satisfies these by construction (the sentinel check is an exact string equality, and the `if err != nil { return err }` in `createOrReplace` already propagates unchanged). If `TestCreateOrReplaceGetErrorPropagatesUnchanged` doesn't fail the way described (e.g. the backend treats an empty name as NotFound instead of a distinct error), adjust the test to use whatever malformed key genuinely produces a non-NotFound error from `s.Get` in this backend, verified by first calling `store.Get` (not `Create`) directly with candidate malformed keys and observing the actual error — do not weaken the assertion to "any error" without first confirming what real error is reachable.

- [ ] **Step 4: Re-run the Task 1 regression baseline**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreate$|TestCreateWithKeyExist$' -v`

Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add pkg/storage/unified/apistore/store_test.go
git commit -m "apistore: pin non-sentinel Create behavior and Get-error propagation"
```

---

### Task 3: Provisioning-lock rejection (Repo-managed resource)

**Files:**
- Test: `pkg/storage/unified/apistore/store_test.go`

**Interfaces:**
- Consumes: `createOrReplace` (Task 1), `utils.ManagerProperties`/`utils.ManagerKindRepo` (existing, `pkg/apimachinery/utils`), `checkManagerPropertiesOnUpdateSpec`/`enforceManagerProperties` (existing, `pkg/storage/unified/apistore/managed.go:65-217` — reached automatically via `GuaranteedUpdate`, no new code to write).

- [ ] **Step 1: Write the failing test**

Add to `store_test.go`:

```go
func TestCreateOrReplaceRejectsRepoManagedResource(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t)
	defer destroyFunc()
	require.NoError(t, err)

	key := "/pods/test-ns/repo-managed"
	first := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "repo-managed", Namespace: "test-ns"}}
	firstMeta, err := utils.MetaAccessor(first)
	require.NoError(t, err)
	firstMeta.SetManagerProperties(utils.ManagerProperties{
		Kind:     utils.ManagerKindRepo,
		Identity: "test-repo",
	})
	firstOut := &example.Pod{}
	require.NoError(t, store.Create(ctx, key, first, firstOut, 0))

	second := &example.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "repo-managed", Namespace: "test-ns", ResourceVersion: apistore.OverwriteOnCreateResourceVersion,
	}}
	secondMeta, err := utils.MetaAccessor(second)
	require.NoError(t, err)
	secondMeta.SetManagerProperties(utils.ManagerProperties{
		Kind:     utils.ManagerKindRepo,
		Identity: "test-repo",
	})

	secondOut := &example.Pod{}
	err = store.Create(ctx, key, second, secondOut, 0)
	require.Error(t, err, "the default test identity is not a provisioning service identity, so this must be rejected exactly like a normal Update to a repo-managed resource would be")
	require.True(t, apierrors.IsForbidden(err))
}
```

Note: this test relies on the default test identity set up in `store_test.go`'s `init()` (`identity.StaticRequester{..., IsGrafanaAdmin: true}`) — being a Grafana admin does *not* make it a provisioning-service identity, so `enforceManagerProperties`'s `ManagerKindRepo` branch (`managed.go:191-196`) rejects it unconditionally. No new test identity is needed for this case.

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./pkg/storage/unified/apistore/... -run TestCreateOrReplaceRejectsRepoManagedResource -v`

Expected: fails with `NoError` — before this task, `Create`'s sentinel branch exists (from Task 1) but nothing in the test has changed that path's behavior, so this should actually already pass if Task 1 is implemented correctly, since the rejection comes entirely from existing `GuaranteedUpdate` machinery. If it fails for a *different* reason than expected (e.g. a panic, or the wrong error type), that's a real bug in how `createOrReplace` reaches `GuaranteedUpdate` — investigate before proceeding, don't assume the test is just mis-written.

- [ ] **Step 3: If it already passes, no implementation step is needed — just confirm and move on**

This task may turn out to require no new code at all, which is the point: it's proving that a safety property requested in the spec (provisioning-lock enforcement) comes for free from routing through `GuaranteedUpdate`, as designed in Task 1. If it fails, the fix belongs in Task 1's `createOrReplace` (e.g. if error handling there is swallowing the real error), not in this test.

- [ ] **Step 4: Re-run the Task 1 regression baseline**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreate$|TestCreateWithKeyExist$' -v`

Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add pkg/storage/unified/apistore/store_test.go
git commit -m "apistore: prove sentinel create-or-replace honors provisioning locks"
```

---

### Task 4: RBAC rejection (caller lacks update rights)

**Files:**
- Modify: `pkg/storage/unified/apistore/watcher_test.go:62-87` (new `setupOption`)
- Test: `pkg/storage/unified/apistore/store_test.go`

**Interfaces:**
- Produces: `withAccessClient(ac claims.AccessClient) setupOption` in `watcher_test.go`, usable by `testSetup(t, withAccessClient(...))`.
- Consumes: `createOrReplace` (Task 1), `claims.FixedAccessClient` (existing, `github.com/grafana/authlib/types` — `FixedAccessClient(false)` denies every check).

This is the task the plan's research flagged as needing investigation: whether the existing in-process test harness (`testSetup` in `watcher_test.go`) can exercise a real permission denial at all, since it omits `AccessClient` from `resource.ResourceServerOptions{Backend: backend}`, which defaults to `claims.FixedAccessClient(true)` — allow-everything (`pkg/storage/unified/resource/server.go:600-601`). The answer, confirmed this session: yes — `claims.FixedAccessClient(false)` already exists as a ready-made deny-everything implementation; the harness just needs a way to pass one in.

- [ ] **Step 1: Add the `withAccessClient` setup option**

In `pkg/storage/unified/apistore/watcher_test.go`, add `"github.com/grafana/authlib/types"` to the import block as `claims`:

```go
	claims "github.com/grafana/authlib/types"
```

Add a field to `setupOptions` (currently lines 62-70):

```go
type setupOptions struct {
	codec          runtime.Codec
	newFunc        func() runtime.Object
	newListFunc    func() runtime.Object
	prefix         string
	resourcePrefix string
	groupResource  schema.GroupResource
	storageType    StorageType
	accessClient   claims.AccessClient
}
```

Add a new option function right after `withStorageType` (currently lines 83-87):

```go
func withAccessClient(ac claims.AccessClient) setupOption {
	return func(options *setupOptions, t testing.TB) {
		options.accessClient = ac
	}
}
```

In `testSetup`'s `StorageTypeFile` branch, thread it through to `resource.NewResourceServer` (currently `resource.ResourceServerOptions{Backend: backend}`, around line 127-129):

```go
		server, err = resource.NewResourceServer(resource.ResourceServerOptions{
			Backend:      backend,
			AccessClient: setupOpts.accessClient,
		})
```

(Passing a `nil` `claims.AccessClient` when `withAccessClient` isn't used is fine — `resource.NewResourceServer` already defaults a `nil` `AccessClient` to `claims.FixedAccessClient(true)` at `server.go:600-601`, so every existing test's behavior is unchanged.)

- [ ] **Step 2: Write the failing test**

Add to `store_test.go`:

```go
func TestCreateOrReplaceRejectsWhenCallerLacksUpdateRights(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t, withAccessClient(claims.FixedAccessClient(false)))
	defer destroyFunc()
	require.NoError(t, err)

	key := "/pods/test-ns/no-update-rights"
	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "no-update-rights", Namespace: "test-ns", ResourceVersion: apistore.OverwriteOnCreateResourceVersion,
	}}
	out := &example.Pod{}
	err = store.Create(ctx, key, obj, out, 0)
	require.Error(t, err, "deny-everything AccessClient should block even the first Create attempt, confirming the harness genuinely enforces access control end to end")
}
```

Note: with `FixedAccessClient(false)`, *every* check fails, including the plain create path (there's no existing object to find yet, so this test can't isolate "found but can't update" from "can't even create" using this blunt a deny-client). That's fine for proving the harness can exercise real denials at all (this step), but Step 3 below needs a sharper test to prove the specific claim — that an *existing* object specifically requires update rights, not just that access control works in general.

- [ ] **Step 3: Run it, confirm it fails, then implement, then write the sharper test**

Run: `go test ./pkg/storage/unified/apistore/... -run TestCreateOrReplaceRejectsWhenCallerLacksUpdateRights -v`

Expected: compile error (`withAccessClient` doesn't exist yet) until Step 1's code is added; once added, the test should pass immediately (deny-everything blocks the plain create, no new product code needed).

Then write the sharper test that actually isolates the "found, but lacks update rights" case using a custom `claims.AccessClient` that allows `VerbCreate` but denies `VerbUpdate`:

```go
type createOnlyAccessClient struct{}

func (createOnlyAccessClient) Check(_ context.Context, _ claims.AuthInfo, req claims.CheckRequest, _ string) (claims.CheckResponse, error) {
	return claims.CheckResponse{Allowed: req.Verb == utils.VerbCreate}, nil
}

func (createOnlyAccessClient) Compile(_ context.Context, _ claims.AuthInfo, _ claims.ListRequest) (claims.ItemChecker, claims.Zookie, error) {
	return func(_, _ string) bool { return true }, &claims.NoopZookie{}, nil
}

func (createOnlyAccessClient) BatchCheck(_ context.Context, _ claims.AuthInfo, req claims.BatchCheckRequest) (claims.BatchCheckResponse, error) {
	return claims.BatchCheckResponse{}, nil
}

func TestCreateOrReplaceRejectsUpdateWithoutUpdateRights(t *testing.T) {
	ctx, store, destroyFunc, err := testSetup(t, withAccessClient(createOnlyAccessClient{}))
	defer destroyFunc()
	require.NoError(t, err)

	key := "/pods/test-ns/create-only"
	first := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "create-only", Namespace: "test-ns"}}
	firstOut := &example.Pod{}
	require.NoError(t, store.Create(ctx, key, first, firstOut, 0), "create-only access should still allow a genuine first create")

	second := &example.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "create-only", Namespace: "test-ns", ResourceVersion: apistore.OverwriteOnCreateResourceVersion,
	}}
	secondOut := &example.Pod{}
	err = store.Create(ctx, key, second, secondOut, 0)
	require.Error(t, err, "the object exists, so this must now require VerbUpdate, which this AccessClient denies")
	require.True(t, apierrors.IsForbidden(err))
}
```

Check `claims.AccessClient`'s exact interface (`AccessChecker` + `AccessLister` in `github.com/grafana/authlib/types`, confirmed this session at that module's `types/access.go:205-236`) before finalizing this fake's method set — the three methods above (`Check`, `Compile`, `BatchCheck`) matched what `FixedAccessClient`'s own `fixedClient` implements in that same file as of this session; if the interface has grown a method since, add a matching no-op/allow implementation for it too, don't leave the fake failing to compile.

- [ ] **Step 4: Run both new tests, confirm pass**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreateOrReplaceRejectsWhenCallerLacksUpdateRights|TestCreateOrReplaceRejectsUpdateWithoutUpdateRights' -v`

Expected: `PASS` for both.

- [ ] **Step 5: Re-run the Task 1 regression baseline**

Run: `go test ./pkg/storage/unified/apistore/... -run 'TestCreate$|TestCreateWithKeyExist$' -v`

Expected: `PASS` — confirms the new `accessClient` field's `nil` default genuinely doesn't change any existing test's behavior.

- [ ] **Step 6: Commit**

```bash
git add pkg/storage/unified/apistore/watcher_test.go pkg/storage/unified/apistore/store_test.go
git commit -m "apistore: prove sentinel create-or-replace re-authorizes as an update"
```

---

### Task 5: Integration test proving the mechanism is generic (folders, not dashboards)

**Files:**
- Create: `pkg/tests/apis/folder/overwrite_test.go`

**Interfaces:**
- Consumes: `apistore.OverwriteOnCreateResourceVersion` (Task 1), the `apis.NewK8sTestHelper`/`helper.GetResourceClient` harness already used throughout `pkg/tests/apis/folder/folders_test.go`.

- [ ] **Step 1: Write the integration test**

Create `pkg/tests/apis/folder/overwrite_test.go`:

```go
package folder

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	folders "github.com/grafana/grafana/apps/folder/pkg/apis/folder/v1"
	"github.com/grafana/grafana/pkg/apimachinery/utils"
	"github.com/grafana/grafana/pkg/storage/unified/apistore"
	"github.com/grafana/grafana/pkg/tests/apis"
	"github.com/grafana/grafana/pkg/tests/testinfra"
	"github.com/grafana/grafana/pkg/util/testutil"
)

func TestIntegrationFolderOverwriteOnCreate(t *testing.T) {
	testutil.SkipIntegrationTestInShortMode(t)

	newFolder := func(name, title string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]any{"title": title},
		}}
		obj.SetName(name)
		obj.SetAPIVersion(folders.GROUP + "/" + folders.VERSION)
		obj.SetKind("Folder")
		return obj
	}

	t.Run("second create with sentinel RV replaces instead of 409", func(t *testing.T) {
		helper := apis.NewK8sTestHelper(t, testinfra.GrafanaOpts{DisableAnonymous: true})
		t.Cleanup(helper.Shutdown)

		ctx := context.Background()
		client := helper.GetResourceClient(apis.ResourceClientArgs{
			User: helper.Org1.Admin,
			GVR:  gvr,
		})

		first := newFolder("overwrite-folder-uid", "first title")
		_, err := client.Resource.Create(ctx, first, metav1.CreateOptions{})
		require.NoError(t, err)

		second := newFolder("overwrite-folder-uid", "second title")
		second.SetResourceVersion(apistore.OverwriteOnCreateResourceVersion)

		updated, err := client.Resource.Create(ctx, second, metav1.CreateOptions{})
		require.NoError(t, err)
		require.Equal(t, "second title", updated.Object["spec"].(map[string]any)["title"])

		fetched, err := client.Resource.Get(ctx, "overwrite-folder-uid", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "second title", fetched.Object["spec"].(map[string]any)["title"])
	})

	t.Run("second create without sentinel RV still 409s", func(t *testing.T) {
		helper := apis.NewK8sTestHelper(t, testinfra.GrafanaOpts{DisableAnonymous: true})
		t.Cleanup(helper.Shutdown)

		ctx := context.Background()
		client := helper.GetResourceClient(apis.ResourceClientArgs{
			User: helper.Org1.Admin,
			GVR:  gvr,
		})

		first := newFolder("no-overwrite-folder-uid", "first title")
		_, err := client.Resource.Create(ctx, first, metav1.CreateOptions{})
		require.NoError(t, err)

		second := newFolder("no-overwrite-folder-uid", "second title")
		_, err = client.Resource.Create(ctx, second, metav1.CreateOptions{})
		require.True(t, errors.IsAlreadyExists(err), "expected AlreadyExists, got: %v", err)
	})

	t.Run("second create with sentinel RV, caller lacks update rights, is rejected", func(t *testing.T) {
		helper := apis.NewK8sTestHelper(t, testinfra.GrafanaOpts{DisableAnonymous: true})
		t.Cleanup(helper.Shutdown)

		ctx := context.Background()
		adminClient := helper.GetResourceClient(apis.ResourceClientArgs{
			User: helper.Org1.Admin,
			GVR:  gvr,
		})
		viewerClient := helper.GetResourceClient(apis.ResourceClientArgs{
			User: helper.Org1.Viewer,
			GVR:  gvr,
		})

		first := newFolder("viewer-overwrite-folder-uid", "first title")
		_, err := adminClient.Resource.Create(ctx, first, metav1.CreateOptions{})
		require.NoError(t, err)

		second := newFolder("viewer-overwrite-folder-uid", "second title")
		second.SetResourceVersion(apistore.OverwriteOnCreateResourceVersion)

		_, err = viewerClient.Resource.Create(ctx, second, metav1.CreateOptions{})
		require.True(t, errors.IsForbidden(err), "expected Forbidden, got: %v", err)
	})
}
```

This file lives in `package folder` (matching `folders_test.go`'s package in the same directory) and reuses that file's package-level `gvr` var (`schema.GroupVersionResource{Group: folders.GROUP, Version: folders.VERSION, Resource: "folders"}`, defined in `folders_test.go:54-58`) rather than redefining it — check that file before writing this one to confirm the var's exact name and visibility haven't changed.

- [ ] **Step 2: Run it**

Run: `go test ./pkg/tests/apis/folder/... -run TestIntegrationFolderOverwriteOnCreate -v`

Expected: `PASS`, all three sub-tests green. If the first sub-test fails with `AlreadyExists` instead of succeeding, re-check that `apistore.OverwriteOnCreateResourceVersion` is actually reaching the server unmodified through the dynamic client's JSON encoding (`unstructured.SetResourceVersion` stores it as a plain string field, so this should just work, but confirm by logging the actual request body if the failure is puzzling). If the third sub-test fails to reject, re-check Task 4's reasoning actually holds end-to-end through the real apiserver, not just the lower-level harness.

- [ ] **Step 3: Commit**

```bash
git add pkg/tests/apis/folder/overwrite_test.go
git commit -m "folder: add integration test proving create-or-replace is generic"
```

---

## Notes for the implementer

- This plan intentionally does not touch `terraform-provider-grafana`, quota admission, or the `ManagerKindTerraform`/`ManagerKindKubectl` `AllowsEdits` gap — see the spec's Non-goals section. Don't fold fixes for any of these in "while you're in there."
- The abandoned branch `feat/v2-replace-true-dashboard` (PR #133751) used a completely different, dashboard-specific mechanism (an annotation + feature toggle at the REST layer). Nothing in it should be reused here beyond the investigative facts already folded into this plan and its spec.
