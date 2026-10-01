# Design: generic create-or-replace via sentinel resourceVersion

**Status:** proposed
**Author:** Charandas Batra
**Date:** 2026-10-01
**Related:** [support-escalations#24320](https://github.com/grafana/support-escalations/issues/24320) (Pismo, ARR $1.27M, P2) · [terraform-provider-grafana#2579](https://github.com/grafana/terraform-provider-grafana/issues/2579) · supersedes the dashboard-specific approach on branch `feat/v2-replace-true-dashboard` (PR [#133751](https://github.com/grafana/grafana/pull/133751), abandoned per lead-developer review — see "History" below)

## Problem

Terraform's `grafana_apps_dashboard_dashboard_v2`/`v2beta1` resources set `options { overwrite = true }`, but `terraform apply` against a dashboard UID that already exists (created via the UI, never imported into Terraform state) fails with `409 AlreadyExists`. See the prior spec (`2026-09-25-dashboard-v2-create-overwrite-design.md`, same escalation) for the original root-cause writeup. This spec corrects and generalizes that analysis.

## Root cause, corrected

The prior spec's root-cause section was imprecise about *why* `overwrite` doesn't help. Precisely, verified in both repos' actual source:

- `terraform-provider-grafana/internal/resources/appplatform/resource.go`: the generic `appplatform.Resource[T,L]` framework backing `grafana_apps_dashboard_dashboard_v2`/`v2beta1` has two lifecycle methods. `createModel` (`resource.go:498-558`) calls `client.Create(ctx, obj, sdkresource.CreateOptions{})` and never reads `Overwrite` at all. `updateModel` (`resource.go:613-690`) is the only place `Overwrite` is read — when true, it clears `UpdateOptions.ResourceVersion` before a `client.Update` (PUT), bypassing optimistic-concurrency.
- Terraform only calls `Create` when the resource isn't yet in its own state. Pismo's dashboards exist in Grafana but were never imported, so Terraform always hits `createModel` — `overwrite=true` is dead code for this exact scenario, not "implemented but broken."
- Confirmed in the provider's other, legacy `grafana_dashboard` resource (`internal/resources/grafana/resource_dashboard.go`) for contrast: it calls `client.Dashboards.PostDashboard(&dashboard)` → `POST /api/dashboards/db`, with `overwrite` mapped straight into `dashboards.SaveDashboardCommand.Overwrite` (`pkg/services/dashboards/models.go:261`), honored by `saveDashboardViaK8s` (`pkg/api/dashboard.go`). This *is* a working get-then-create-or-update path — but it's the legacy endpoint, a different TF resource type entirely, not what Pismo uses.
- Empirically verified against a real apiserver (throwaway probes this session, not committed): a `Create` for an existing name with `resourceVersion` left empty produces the same plain `409 AlreadyExists` as never setting it. A `Create` with a *non-empty* RV is rejected even earlier, with `storage.ErrResourceVersionSetOnCreate` ("resourceVersion should not be set on objects to be created"), from `pkg/storage/unified/apistore/prepare.go:158-159`. So "empty RV" carries zero information today — it's the mandatory state of every Create request, overwrite-intending or not, and any other value is actively rejected.

## History: why this supersedes the dashboard-specific PR

PR #133751 (branch `feat/v2-replace-true-dashboard`) implemented an annotation (`grafana.app/overwrite-existing`) + feature toggle, intercepted in `dashboardStorageWrapper.Create` — a `rest.Storage`-layer wrapper specific to the dashboard resource. Its own spec explicitly chose dashboard-specific over a shared apistore change, reasoning that no hook point existed in the shared store and that behavior should be validated on one resource before asking App Platform to bless a platform-wide capability.

A Grafana lead developer reviewed that PR's premise and pushed back: the real mechanism Terraform already half-implements (`Overwrite` → empty `ResourceVersion` on Update) suggests the fix belongs on the `resourceVersion` itself, done once, generically, for every resource — "I am happy to even use a special RV like '-1' or something (that seems better than custom annotations)." Investigation this session found the actual generic hook point does exist (`prepare.go:158-159`, previously missed), making the annotation's premise (there's no hook point for this) incorrect. Branch `feat/v2-replace-true-dashboard` and PR #133751 are abandoned; this spec's work happens on a fresh branch (`dashboard-overwrite-rv-design`) off latest `main`.

## Decision: generic, inside apistore, no feature toggle

Implement entirely within `pkg/storage/unified/apistore/` (`store.go`'s `Storage.Create`, `prepare.go`), below the REST/authz layer, applying to every app-sdk resource on unified storage automatically. No protobuf/RPC contract change, no per-app wrapper, no feature toggle — the sentinel value is new and today only produces a validation error for every caller, so recognizing it cannot change any existing caller's behavior; there's nothing to dark-launch.

### Why not the alternative (a shared `rest.Storage`-layer wrapper, above authz)

Considered generalizing `dashboardStorageWrapper` into a reusable decorator installed at the app-sdk builder layer for every app, which would sit above the REST authorizer the way a normal Update naturally does. Rejected in favor of the apistore-internal approach once investigation showed the apparent safety advantage was illusory to need: `GuaranteedUpdate` (which the apistore approach already must call) issues a real `Update` RPC to the storage server, and `server.update` (`pkg/storage/unified/resource/server.go:1354-1364`) independently re-authorizes `VerbUpdate` server-side, generically, for any resource type, regardless of which client-side Go path reached it (confirmed by reading the code: `store.go:820`, `s.store.Update(ctx, req) // Also does RBAC check`). The apistore-internal approach gets the same safety property for free, with a far smaller, better-contained diff (one file's internals vs. new shared builder-layer infrastructure every app would need to adopt).

## Design

### Sentinel value

`"-1"`, per the lead developer's suggestion. Checked for collisions: no existing code in `pkg/storage/unified/apistore` or `pkg/apimachinery/utils` treats `"-1"` specially today. Exact constant name and home package TBD at implementation time (likely `apistore`, since it's the only package that reads it) — not a design-level decision worth blocking on.

### Mechanics

In `Storage.Create` (`store.go:319`), before the existing call to `s.prepareObjectForStorage`:

1. Read `meta.GetResourceVersion()`. If it equals the sentinel: clear it (`meta.SetResourceVersion("")`) and branch into a new `createOrReplace` path. Any other value (empty, or any other non-empty string) is completely unaffected — behavior is byte-for-byte what it is today.
2. `createOrReplace`: `s.Get` the object by key.
   - `NotFound` → fall through to the existing create path (now with RV legitimately empty).
   - Any other error → propagate unchanged. Never mask a real error (e.g. `Forbidden` on a `Get` for an object in a namespace/folder the caller can't read) behind a synthetic conflict.
   - Found → build a `storage.UpdateFunc` that returns the incoming object as-is (full replace — "declared state wins"), and call `s.GuaranteedUpdate(ctx, key, out, false, nil /* no preconditions: blind overwrite, matching what Overwrite already means on the Update path today */, tryUpdate, nil)`.

Everything past that point — provisioning-lock enforcement (`prepareObjectForUpdate` → `checkManagerPropertiesOnUpdateSpec`, `managed.go:65`) and the independent server-side RBAC check (`GuaranteedUpdate` → `s.store.Update` → `server.update`'s `s.access.Check(VerbUpdate, ...)`) — is the exact same code path every normal Update already goes through. No new logic needed for either.

### Why Get-first, not optimistic-create-then-fallback

The prior (abandoned) PR learned the hard way that an optimistic-create-first approach at the REST layer re-runs CREATE-flavored admission (including quota) against an object that already exists, blocking overwrite exactly when an org is at quota. At *this* layer that specific failure mode doesn't recur for the same reason — admission/quota already ran once, upstream of `apistore.Storage.Create`, before this code is ever reached, regardless of which internal strategy is used here. Get-first is still the better choice: it matches the realistic workload (a bulk Terraform import of already-existing dashboards — usually found, rarely not), avoids a doomed RPC round-trip in the common case, and stays consistent with the established pattern in `saveDashboardViaK8s`.

### Error handling summary

| Scenario | Behavior |
|---|---|
| RV empty or any non-sentinel value | Unchanged: existing behavior exactly as today |
| Sentinel RV, name doesn't exist | Normal create |
| Sentinel RV, name exists, editable, caller has update rights | Replaced via `GuaranteedUpdate` |
| Sentinel RV, name exists, Repo-managed (Git Sync) | Rejected — `errResourceIsManagedInRepository`, unconditional for non-provisioning callers (existing, generic, already stricter than the abandoned PR assumed) |
| Sentinel RV, name exists, caller lacks update rights | Rejected — real server-side `VerbUpdate` check, same as any Update |
| Sentinel RV, `Get` fails for a reason other than NotFound (e.g. Forbidden) | That error propagates unchanged |

### Testing

- Unit tests in `pkg/storage/unified/apistore/store_test.go`: sentinel + not-found → creates; sentinel + found + editable → replaces (assert `GuaranteedUpdate`'s underlying write fires, not a second create); sentinel + found + Repo-managed → rejected; sentinel + found + caller lacks update rights → Forbidden propagates; non-sentinel values (empty and otherwise) → behavior provably unchanged from today.
- Integration test(s) against a real apiserver, generalized beyond dashboards if a second resource type is easy to exercise in the existing `pkg/tests/apis/` harness (e.g. folders) — proving the "generic, not dashboard-specific" claim with evidence rather than asserting it from the architecture alone.

## Non-goals (explicit, deferred)

- **Quota admission.** Still runs as CREATE-flavored admission before `apistore.Storage.Create` is ever invoked — an org at quota still can't overwrite an existing object via the sentinel, same failure mode the abandoned PR hit, just now potentially affecting every resource type's own quota check instead of only dashboards'. Fixing this means each resource's own quota admission plugin (e.g. `pkg/registry/apis/dashboard/register.go`'s quota check) recognizing the sentinel and skipping when the target already exists — a per-resource-type follow-up, not a generic apistore change.
- **The `ManagerKindTerraform`/`ManagerKindKubectl` `AllowsEdits` no-op.** `managed.go:205-210` has a standing `// TODO: check the kubectl+terraform resource` — `AllowsEdits == false` is currently not enforced for these manager kinds, for *any* Update, sentinel-triggered or not. Pre-existing gap, not introduced or worsened here, but worth flagging since this design routes more traffic through the same code.
- **`terraform-provider-grafana`'s `createModel`.** Making it actually send the sentinel RV when `opts.Overwrite` is true is a separate PR/plan in that repo, once this backend mechanism exists to call into.

## Open questions

- Exact sentinel constant's name and home package — implementation detail, not a design blocker.
- Whether `errResourceIsManagedInRepository`'s unconditional rejection (no `AllowsEdits` consideration at all for Repo-managed resources) is the right behavior specifically for the sentinel-triggered path, or whether it should be identical to normal Update's behavior (which this design assumes, since it's the exact same code path) — flagging in case App Platform wants Repo-managed resources to behave differently when the write originates from this new mechanism specifically. Default assumption here: no special-casing, identical to normal Update.
