#!/usr/bin/env python3
"""Helm chart render tests for hyperping-exporter.

Drives `helm template -f <fixture>` for each test case, parses the rendered
YAML, and asserts:

  - The Deployment's first container args list matches an expected list
    exactly. This literal comparison is the harness's load-bearing
    apply-time-failure detector: any template-side regression in quoting,
    escaping, or flag ordering surfaces here.
  - The chart-managed Secret is present or absent according to whether
    inline apiKey is set (existingSecret-only path).
  - Every string-typed scalar in the rendered manifest is a real Python
    str and JSON-encodes cleanly (a smoke check that decode produced
    well-formed strings; not a strict-mode YAML validator and not a
    substitute for `kubectl apply --dry-run=client`).

PyYAML is the only third-party dependency. helm must be on PATH; when it
is not, this script exits with code 2 and prints a clear install hint.
Exit code 0 on success, 1 on any assertion failure.

TDD pattern for new render-test cases: add (or update) the fixture first
so the existing harness goes red against the missing fixture or stale
assertion, then land the chart change and update the expected-args
literal so the harness goes green. This keeps each case load-bearing
(a template-side regression flips the assertion) rather than tautological.
"""
from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
CHART = HERE.parent              # deploy/helm/hyperping-exporter
FIXTURES = HERE / "fixtures"
HELM = "helm"


def helm_template(fixture: str) -> str:
    """Render the chart with `-f <fixture>` and return rendered YAML."""
    return subprocess.check_output(
        [HELM, "template", "testrel", str(CHART), "-f", str(FIXTURES / fixture)],
        text=True,
    )


def docs(rendered: str) -> list[dict]:
    return [d for d in yaml.safe_load_all(rendered) if d]


def find_deployment(rendered: str) -> dict:
    for d in docs(rendered):
        if d.get("kind") == "Deployment":
            return d
    raise AssertionError("Deployment not found")


def find_secret(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "Secret":
            return d
    return None


def find_external_secret(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "ExternalSecret":
            return d
    return None


def find_pdb(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "PodDisruptionBudget":
            return d
    return None


def find_network_policy(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "NetworkPolicy":
            return d
    return None


def find_cilium_network_policy(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "CiliumNetworkPolicy":
            return d
    return None


def deployment_env(rendered: str) -> list[dict]:
    return (
        find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0]
        .get("env", [])
    )


def assert_fail(case_name: str, fixture: str, expected_stderr_substring: str,
                forbidden_stdout_substring: str | None = None) -> None:
    """Run helm template and assert it exits non-zero AND stderr contains the
    given substring. Optionally assert a forbidden substring is absent from
    captured stdout (leakage-proof per Contract C8.1)."""
    proc = subprocess.run(
        [HELM, "template", "testrel", str(CHART), "-f", str(FIXTURES / fixture)],
        capture_output=True, text=True,
    )
    if proc.returncode == 0:
        print(f"FAIL {case_name}: expected helm template to fail; got returncode 0", file=sys.stderr)
        print(f"  stdout: {proc.stdout[:500]}", file=sys.stderr)
        sys.exit(1)
    if expected_stderr_substring not in proc.stderr:
        print(f"FAIL {case_name}: expected stderr to contain {expected_stderr_substring!r}", file=sys.stderr)
        print(f"  stderr: {proc.stderr[:1000]}", file=sys.stderr)
        sys.exit(1)
    if forbidden_stdout_substring is not None:
        if forbidden_stdout_substring in proc.stdout:
            print(f"FAIL {case_name}: forbidden substring {forbidden_stdout_substring!r} leaked into stdout", file=sys.stderr)
            print(f"  stdout: {proc.stdout[:1000]}", file=sys.stderr)
            sys.exit(1)
    print(f"PASS {case_name}: assert_fail({expected_stderr_substring!r}) rc={proc.returncode}")


def deployment_args(rendered: str) -> list[str]:
    return find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0]["args"]


def find_all(rendered: str, kind: str) -> list[dict]:
    """Return every parsed document matching the given Kubernetes kind."""
    return [d for d in docs(rendered) if d.get("kind") == kind]


def deployment_image(rendered: str) -> str:
    return find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0]["image"]


def labels_with_version(rendered: str) -> dict[str, str]:
    """Map each rendered resource that carries `app.kubernetes.io/version`
    to its value, keyed by `<kind>/<metadata.name>`. Useful so a regression
    pinpoints exactly which resource's labels block drifted.
    """
    out: dict[str, str] = {}
    for d in docs(rendered):
        labels = (d.get("metadata") or {}).get("labels") or {}
        v = labels.get("app.kubernetes.io/version")
        if v is not None:
            key = f"{d.get('kind')}/{d['metadata'].get('name')}"
            out[key] = v
    return out


def assert_scalars_clean(rendered: str, label: str) -> None:
    """Smoke check that the YAML decoder produced Python `str` scalars
    (not bytes, not surrogate-bearing objects) and that each such scalar
    JSON-encodes without error.

    What this is NOT: a kubectl apply --dry-run=client substitute. PyYAML
    is lenient and will accept inputs that stricter YAML decoders (such
    as the one Kubernetes uses) would reject; this walk catches neither
    that class of bug nor schema-level violations. It is purely a check
    that `helm template` produced parseable text whose string scalars are
    well-formed Python strings.

    The real apply-time-failure detection in this harness is the literal
    `assert_eq(deployment_args(...), BASELINE_ARGS + [...])` comparisons:
    those fix the rendered container args list to an exact expected value
    so any quoting, escaping, or template-side regression on the contract
    surfaces immediately.
    """
    def walk(node):
        if isinstance(node, dict):
            for v in node.values():
                walk(v)
        elif isinstance(node, list):
            for v in node:
                walk(v)
        elif isinstance(node, str):
            # Will raise on un-encodable strings; success here means the
            # scalar is a real Python str with no parser-confused bytes.
            json.dumps(node)
    for d in docs(rendered):
        walk(d)
    print(f"PASS {label}: rendered scalars are clean")


def assert_eq(actual, expected, label: str) -> None:
    if actual != expected:
        print(f"FAIL {label}", file=sys.stderr)
        print(f"  expected: {expected!r}", file=sys.stderr)
        print(f"  actual:   {actual!r}", file=sys.stderr)
        sys.exit(1)
    print(f"PASS {label}")


# Baseline args list when only required values are set. Order matters and
# matches templates/deployment.yaml. The chart's `service.port` default is
# 9312; metricsPath default `/metrics`; cacheTTL `60s`; logLevel `info`;
# logFormat `text`. Any change to these defaults requires updating this
# constant (and is itself a behaviour change reviewers should notice).
BASELINE_ARGS = [
    "--listen-address=:9312",
    "--metrics-path=/metrics",
    "--cache-ttl=60s",
    "--log-level=info",
    "--log-format=text",
]


# Default image and version contract. The chart's `image.repository`
# default must resolve on Docker Hub, and `appVersion` must match a tag
# that actually understands the flags this chart now renders
# (`--mcp-url` since v1.2.0, `--exclude-name-pattern` since v1.3.0).
# Published binary: khaledsalhabdeveleap/hyperping-exporter:1.4.0.
EXPECTED_IMAGE_DEFAULT = "khaledsalhabdeveleap/hyperping-exporter:1.4.1"
EXPECTED_VERSION = "1.4.1"
EXPECTED_CHART_LABEL = "hyperping-exporter-1.5.0"


def main() -> int:
    if shutil.which(HELM) is None:
        print("ERROR: helm binary not on PATH. Install Helm v3.x.", file=sys.stderr)
        return 2

    # Case 1 — defaults: args list is exactly the baseline (contract 2).
    rendered = helm_template("default.values.yaml")
    assert_eq(deployment_args(rendered), BASELINE_ARGS,
              "defaults: args list equals baseline (no new flags emitted)")
    assert find_secret(rendered) is not None, \
        "FAIL defaults: chart-managed Secret should be present"
    print("PASS defaults: chart-managed Secret present")
    # Image and version-label contract: defaults must point at the
    # published binary, and every resource that carries the common
    # labels block must advertise the matching `app.kubernetes.io/version`.
    assert_eq(deployment_image(rendered), EXPECTED_IMAGE_DEFAULT,
              "defaults: Deployment image equals published Docker Hub tag")
    versions = labels_with_version(rendered)
    # Expect the label on every chart-labelled resource. Default render after
    # Task 7 emits Secret, Service, Deployment, NetworkPolicy (NP flipped on
    # by default in chart 1.5.0). Use set-difference enumeration so a future
    # additional resource produces a clear "new resource" diff rather than a
    # tuple mismatch (Contract C8.3).
    expected_versions: dict[str, str] = {}
    for kind in ("Secret", "Service", "Deployment", "NetworkPolicy"):
        for d in find_all(rendered, kind):
            expected_versions[f"{kind}/{d['metadata']['name']}"] = EXPECTED_VERSION
    missing = set(expected_versions) - set(versions)
    extra = set(versions) - set(expected_versions)
    if missing or extra:
        print(
            f"FAIL defaults: app.kubernetes.io/version label coverage drift; "
            f"missing={sorted(missing)} extra={sorted(extra)}",
            file=sys.stderr,
        )
        sys.exit(1)
    assert_eq(versions, expected_versions,
              f"defaults: app.kubernetes.io/version label is {EXPECTED_VERSION} on every labelled resource")
    # helm.sh/chart label coverage: every labelled resource carries the
    # chart-version label sourced from the CHART_VERSION anchor.
    chart_labels: dict[str, str] = {}
    for d in docs(rendered):
        labels = (d.get("metadata") or {}).get("labels") or {}
        v = labels.get("helm.sh/chart")
        if v is not None:
            chart_labels[f"{d.get('kind')}/{d['metadata'].get('name')}"] = v
    expected_chart_labels = {k: EXPECTED_CHART_LABEL for k in expected_versions}
    assert_eq(chart_labels, expected_chart_labels,
              f"defaults: helm.sh/chart label is {EXPECTED_CHART_LABEL} on every labelled resource")
    assert_scalars_clean(rendered, "defaults")

    # Case 2 — existingSecret path. No chart-managed Secret rendered; args
    # list is still exactly the baseline.
    rendered = helm_template("existing-secret.values.yaml")
    assert_eq(deployment_args(rendered), BASELINE_ARGS,
              "existing-secret: args list equals baseline")
    assert find_secret(rendered) is None, \
        "FAIL existing-secret: chart-managed Secret must NOT be present"
    print("PASS existing-secret: chart-managed Secret absent (external secret consumed)")
    # Deployment env still references the external secret name.
    env = find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0]["env"]
    secret_ref = env[0]["valueFrom"]["secretKeyRef"]["name"]
    assert_eq(secret_ref, "my-external-secret",
              "existing-secret: container env references the external secret")
    assert_scalars_clean(rendered, "existing-secret")

    # Case 3 — ASCII regex. args list is baseline + exactly one new entry.
    rendered = helm_template("ascii-regex.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + ["--exclude-name-pattern=^prod-"],
              "ascii regex: args list = baseline + one new entry")
    assert_scalars_clean(rendered, "ascii regex")

    # Case 4 — README documented example (contract 1, contract 4, load-bearing).
    # Exact README spelling: '\[DRILL|\[TEST'. The chart transports this
    # verbatim; whether the string is a "correct" RE2 regex is owned by
    # the README, not by the chart.
    pattern = r"\[DRILL|\[TEST"
    rendered = helm_template("readme-regex.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + [f"--exclude-name-pattern={pattern}"],
              "README example: args list contains the exact README literal")
    assert_scalars_clean(rendered, "README example")

    # Case 5 — regex containing a single quote (Option C handles it).
    rendered = helm_template("single-quote-regex.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + ["--exclude-name-pattern=^test'name"],
              "single-quote regex: args list contains literal apostrophe")
    assert_scalars_clean(rendered, "single-quote regex")

    # Case 6 — mcpUrl alone.
    rendered = helm_template("mcp-url.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + ["--mcp-url=https://mcp.example.com/v1/mcp"],
              "mcp-url: args list = baseline + one new entry")
    assert_scalars_clean(rendered, "mcp-url")

    # Case 7 — both flags together. Args list contains both new entries
    # in the order the template renders them (excludeNamePattern first).
    rendered = helm_template("both-flags.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + [
                  f"--exclude-name-pattern={pattern}",
                  "--mcp-url=https://mcp.example.com/v1/mcp",
              ],
              "combo: args list = baseline + both new entries in template order")
    assert_scalars_clean(rendered, "combo")

    # Case 8 — regex containing literal double quotes. YAML single-quoted
    # scalar `'"foo"'` decodes to the string `"foo"` (quotes intact).
    # toJson escapes those quotes; Kubernetes' YAML decoder reverses the
    # escape, so the container sees `--exclude-name-pattern="foo"` as a
    # single arg with embedded quote characters.
    rendered = helm_template("quote-regex.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + ['--exclude-name-pattern="foo"'],
              "quote regex: args list contains literal double quotes")
    assert_scalars_clean(rendered, "quote regex")

    # Case 9 — mcpUrl whose query value embeds a literal `"` and `\`. This
    # is the load-bearing mutation test for the `toJson` rendering of
    # mcpUrl: both characters require JSON escaping, so a naive
    # `"--mcp-url={{ .Values.config.mcpUrl }}"` template would either fail
    # helm's YAML output or decode to a different string. The fixture's
    # single-quoted YAML preserves `\` literally; `toJson` escapes both
    # `"` and `\`; Kubernetes' YAML decoder reverses both escapes, so the
    # container sees the source URL byte-for-byte.
    rendered = helm_template("mcp-url-query.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + [
                  '--mcp-url=https://mcp.example.com/v1/mcp?token="ab\\cd"',
              ],
              "mcp-url query: URL with literal quote and backslash passes through verbatim")
    assert_scalars_clean(rendered, "mcp-url query")

    # ---- v1.5.0 cases ----
    # Case 10 — external-secret positive render.
    rendered = helm_template("external-secret.values.yaml")
    es = find_external_secret(rendered)
    assert es is not None, "FAIL external-secret: ExternalSecret missing"
    assert find_secret(rendered) is None, \
        "FAIL external-secret: chart-managed Secret must NOT render alongside ExternalSecret"
    assert_eq(es["spec"]["secretStoreRef"]["name"], "my-store",
              "external-secret: secretStoreRef.name passes through")
    assert_eq(es["spec"]["target"]["name"], "testrel-hyperping-exporter",
              "external-secret: target.name equals chart fullname")
    print("PASS external-secret: ExternalSecret rendered with chart Secret absent")
    assert_scalars_clean(rendered, "external-secret")

    # Case 11 — external-secret-defaults (default refreshInterval).
    rendered = helm_template("external-secret-defaults.values.yaml")
    es = find_external_secret(rendered)
    assert es is not None, "FAIL external-secret-defaults: ExternalSecret missing"
    assert_eq(es["spec"]["refreshInterval"], "1h",
              "external-secret-defaults: refreshInterval default is 1h")
    assert_scalars_clean(rendered, "external-secret-defaults")

    # Case 12 — external-secret-missing-store assert_fail.
    assert_fail("external-secret-missing-store",
                "external-secret-missing-store.values.yaml",
                "secretStoreRef.name is required")

    # Case 13 — replicas-zero positive render with NO source.
    rendered = helm_template("replicas-zero.values.yaml")
    dep = find_deployment(rendered)
    assert_eq(dep["spec"]["replicas"], 0,
              "replicas-zero: Deployment replicas is 0")
    env = deployment_env(rendered)
    assert all(e.get("name") != "HYPERPING_API_KEY" for e in env), \
        "FAIL replicas-zero: env block must omit HYPERPING_API_KEY when no source set"
    print("PASS replicas-zero: env block suppressed (no secret source)")
    assert find_pdb(rendered) is None, \
        "FAIL replicas-zero: PDB must be absent"
    print("PASS replicas-zero: PDB absent")
    assert_scalars_clean(rendered, "replicas-zero")

    # Case 14 — replicas-multi assert_fail.
    assert_fail("replicas-multi", "replicas-multi.values.yaml",
                "replicaCount must be 0 or 1")

    # Case 15 — apikey + existingSecret conflict.
    assert_fail("secret-conflict-apikey-and-existing",
                "secret-conflict-apikey-and-existing.values.yaml",
                "config.apiKey")
    # Re-check existingSecret named:
    proc = subprocess.run(
        [HELM, "template", "testrel", str(CHART),
         "-f", str(FIXTURES / "secret-conflict-apikey-and-existing.values.yaml")],
        capture_output=True, text=True,
    )
    assert "config.existingSecret" in proc.stderr, \
        f"FAIL secret-conflict-apikey-and-existing: stderr missing existingSecret mention: {proc.stderr[:300]}"
    print("PASS secret-conflict-apikey-and-existing: conflict names both sources")

    # Case 16 — apikey + externalSecret conflict.
    assert_fail("secret-conflict-apikey-and-external",
                "secret-conflict-apikey-and-external.values.yaml",
                "externalSecret.enabled")
    proc = subprocess.run(
        [HELM, "template", "testrel", str(CHART),
         "-f", str(FIXTURES / "secret-conflict-apikey-and-external.values.yaml")],
        capture_output=True, text=True,
    )
    assert "config.apiKey" in proc.stderr, \
        f"FAIL secret-conflict-apikey-and-external: stderr missing apiKey mention: {proc.stderr[:300]}"
    print("PASS secret-conflict-apikey-and-external: conflict names both sources")

    # Case 17 — existingSecret + externalSecret conflict.
    assert_fail("secret-conflict-existing-and-external",
                "secret-conflict-existing-and-external.values.yaml",
                "config.existingSecret")
    proc = subprocess.run(
        [HELM, "template", "testrel", str(CHART),
         "-f", str(FIXTURES / "secret-conflict-existing-and-external.values.yaml")],
        capture_output=True, text=True,
    )
    assert "externalSecret.enabled" in proc.stderr, \
        f"FAIL secret-conflict-existing-and-external: stderr missing externalSecret mention: {proc.stderr[:300]}"
    print("PASS secret-conflict-existing-and-external: conflict names both sources")

    # Case 18 — missing source at replicaCount: 1.
    assert_fail("secret-source-missing", "secret-source-missing.values.yaml",
                "secret-source missing")

    # Case 22 — cache-ttl-numeric (non-default quoted duration).
    rendered = helm_template("cache-ttl-numeric.values.yaml")
    args = deployment_args(rendered)
    expected = [a if not a.startswith("--cache-ttl=") else "--cache-ttl=30s" for a in BASELINE_ARGS]
    assert_eq(args, expected,
              "cache-ttl-numeric: non-default cacheTTL renders byte-clean through helper")
    assert_scalars_clean(rendered, "cache-ttl-numeric")

    # Case 23 — cache-ttl-int-fails (bare int aborts).
    assert_fail("cache-ttl-int-fails", "cache-ttl-int-fails.values.yaml",
                "must be a quoted Go duration")

    # Case 24 — log-level-numeric (typed input renders clean).
    rendered = helm_template("log-level-numeric.values.yaml")
    args = deployment_args(rendered)
    for a in args:
        assert "%!s" not in a, f"FAIL log-level-numeric: Sprig formatting artefact in {a!r}"
        assert isinstance(a, str), f"FAIL log-level-numeric: non-str arg {a!r}"
    # The numeric logLevel coerces to a string in the rendered arg.
    assert any(a.startswith("--log-level=") for a in args), \
        f"FAIL log-level-numeric: no --log-level arg in {args!r}"
    print("PASS log-level-numeric: args contain no %!s artefacts")
    assert_scalars_clean(rendered, "log-level-numeric")

    # Case 25 — metrics-path-with-special-chars (quotes + backslash round-trip).
    rendered = helm_template("metrics-path-with-special-chars.values.yaml")
    args = deployment_args(rendered)
    assert '--metrics-path=/m"e\\trics' in args, \
        f"FAIL metrics-path-special: expected literal special-char path, got args={args!r}"
    print("PASS metrics-path-with-special-chars: special chars round-trip")
    assert_scalars_clean(rendered, "metrics-path-with-special-chars")

    # Case 19 — networkpolicy-default positive render.
    rendered = helm_template("networkpolicy-default.values.yaml")
    np = find_network_policy(rendered)
    assert np is not None, "FAIL networkpolicy-default: NetworkPolicy missing"
    assert find_cilium_network_policy(rendered) is None, \
        "FAIL networkpolicy-default: CiliumNetworkPolicy must be absent"
    egress = np["spec"]["egress"]
    # DNS rule + TCP/443 rule
    assert any(
        any(p.get("port") == 53 for p in r.get("ports", []))
        for r in egress
    ), "FAIL networkpolicy-default: DNS egress rule missing"
    https_rules = [r for r in egress if any(p.get("port") == 443 for p in r.get("ports", []))]
    assert https_rules, "FAIL networkpolicy-default: TCP/443 egress rule missing"
    for r in https_rules:
        for to in r.get("to", []):
            ipb = to.get("ipBlock") or {}
            assert ipb.get("cidr") == "0.0.0.0/0", \
                f"FAIL networkpolicy-default: HTTPS rule cidr drift: {ipb!r}"
            assert "except" not in ipb, \
                f"FAIL networkpolicy-default: HTTPS rule retains except list: {ipb!r}"
    print("PASS networkpolicy-default: vanilla NetworkPolicy egress (DNS + TCP/443 0.0.0.0/0 no except)")
    assert_scalars_clean(rendered, "networkpolicy-default")

    # Case 20 — pdb-enabled at replicaCount=1 with no bypass: PDB suppressed.
    rendered = helm_template("pdb-enabled.values.yaml")
    assert find_pdb(rendered) is None, \
        "FAIL pdb-enabled: PDB must be suppressed at replicaCount=1 without bypass"
    print("PASS pdb-enabled: PDB suppressed at replicaCount=1 (drain-safe gate)")
    assert_scalars_clean(rendered, "pdb-enabled")

    # Case 21 — pdb-structural with internal bypass: PDB renders with required shape.
    rendered = helm_template("pdb-structural.values.yaml")
    pdb = find_pdb(rendered)
    assert pdb is not None, "FAIL pdb-structural: PDB missing under bypass"
    assert_eq(pdb["apiVersion"], "policy/v1",
              "pdb-structural: PDB apiVersion")
    assert_eq(pdb["spec"]["maxUnavailable"], 1,
              "pdb-structural: maxUnavailable default is 1")
    sel_labels = pdb["spec"]["selector"]["matchLabels"]
    assert "app.kubernetes.io/name" in sel_labels and "app.kubernetes.io/instance" in sel_labels, \
        f"FAIL pdb-structural: selector matchLabels missing standard keys: {sel_labels!r}"
    print("PASS pdb-structural: PDB structurally correct under internal bypass")
    assert_scalars_clean(rendered, "pdb-structural")

    # Case 29 — pdb-structural with bypass + replicaCount=2: validator still aborts; no PDB.
    assert_fail(
        "pdb-structural-internal-bypass-multi-replica-fails",
        "pdb-structural-internal-bypass-multi-replica.values.yaml",
        "replicaCount must be 0 or 1",
        forbidden_stdout_substring="kind: PodDisruptionBudget",
    )

    # Case 30 — cilium-egress-only.
    rendered = helm_template("networkpolicy-cilium-defaults.values.yaml")
    cnp = find_cilium_network_policy(rendered)
    assert cnp is not None, "FAIL cilium-egress-only: CiliumNetworkPolicy missing"
    assert find_network_policy(rendered) is None, \
        "FAIL cilium-egress-only: vanilla NetworkPolicy must be absent"
    assert "ingress" not in cnp["spec"], \
        f"FAIL cilium-egress-only: ingress key must be DROPPED (not [])"
    assert "egress" in cnp["spec"], "FAIL cilium-egress-only: egress key required"
    # FQDN block in egress
    fqdn_rules = [r for r in cnp["spec"]["egress"] if "toFQDNs" in r]
    assert fqdn_rules, "FAIL cilium-egress-only: no toFQDNs rule"
    assert any(
        any(f.get("matchName") == "api.hyperping.io" for f in r.get("toFQDNs", []))
        for r in fqdn_rules
    ), "FAIL cilium-egress-only: api.hyperping.io matchName missing"
    print("PASS cilium-egress-only: CiliumNetworkPolicy egress-only with toFQDNs")
    assert_scalars_clean(rendered, "cilium-egress-only")

    # Case 31 — cilium-ingress-matchlabels.
    rendered = helm_template("networkpolicy-cilium-with-ingress.values.yaml")
    cnp = find_cilium_network_policy(rendered)
    assert cnp is not None, "FAIL cilium-matchlabels: CiliumNetworkPolicy missing"
    assert "ingress" in cnp["spec"], "FAIL cilium-matchlabels: ingress key required"
    from_endpoints = cnp["spec"]["ingress"][0]["fromEndpoints"]
    assert from_endpoints[0]["matchLabels"]["app.kubernetes.io/name"] == "prometheus", \
        f"FAIL cilium-matchlabels: matchLabels drift {from_endpoints!r}"
    print("PASS cilium-ingress-matchlabels: matchLabels flatten into EndpointSelector")
    assert_scalars_clean(rendered, "cilium-ingress-matchlabels")

    # Case 32 — cilium-ingress-matchexpressions.
    rendered = helm_template("networkpolicy-cilium-matchexpressions.values.yaml")
    cnp = find_cilium_network_policy(rendered)
    assert cnp is not None, "FAIL cilium-matchexpressions: CiliumNetworkPolicy missing"
    me = cnp["spec"]["ingress"][0]["fromEndpoints"][0].get("matchExpressions")
    assert me, "FAIL cilium-matchexpressions: matchExpressions missing"
    assert me[0]["key"] == "app.kubernetes.io/name" and me[0]["operator"] == "In", \
        f"FAIL cilium-matchexpressions: drift {me!r}"
    print("PASS cilium-ingress-matchexpressions: matchExpressions preserved")
    assert_scalars_clean(rendered, "cilium-ingress-matchexpressions")

    # Case 33 — cilium-ingress-mixed (matchLabels + matchExpressions + ns shortcut).
    rendered = helm_template("networkpolicy-cilium-mixed.values.yaml")
    cnp = find_cilium_network_policy(rendered)
    assert cnp is not None, "FAIL cilium-mixed: CiliumNetworkPolicy missing"
    peer = cnp["spec"]["ingress"][0]["fromEndpoints"][0]
    assert peer["matchLabels"]["tier"] == "monitoring", \
        f"FAIL cilium-mixed: matchLabels lost"
    assert peer["matchLabels"]["k8s:io.kubernetes.pod.namespace"] == "monitoring", \
        f"FAIL cilium-mixed: namespace shortcut conversion lost: {peer!r}"
    assert peer["matchExpressions"][0]["key"] == "app.kubernetes.io/name", \
        f"FAIL cilium-mixed: matchExpressions lost"
    print("PASS cilium-ingress-mixed: matchLabels + matchExpressions + ns shortcut")
    assert_scalars_clean(rendered, "cilium-ingress-mixed")

    # Case 34 — cilium ipBlock-only abort.
    assert_fail("cilium-ipblock-only-fails",
                "networkpolicy-cilium-ipblock-only.values.yaml",
                "ipBlock peers are not supported")

    # Case 35 — cilium selectorless abort.
    assert_fail("cilium-selectorless-fails",
                "networkpolicy-cilium-selectorless.values.yaml",
                "peer must have a podSelector or namespaceSelector")

    # Case 36 — cilium foreign-ns-label abort.
    assert_fail("cilium-foreign-ns-label-fails",
                "networkpolicy-cilium-foreign-namespace-label.values.yaml",
                "k8s:io.kubernetes.pod.namespace.labels")

    # Case 37 — servicemonitor enabled (template coverage).
    rendered = helm_template("servicemonitor-enabled.values.yaml")
    sm = [d for d in docs(rendered) if d.get("kind") == "ServiceMonitor"]
    assert sm, "FAIL servicemonitor-enabled: ServiceMonitor missing"
    assert_eq(sm[0]["apiVersion"], "monitoring.coreos.com/v1",
              "servicemonitor-enabled: apiVersion")
    assert_eq(sm[0]["spec"]["endpoints"][0]["interval"], "30s",
              "servicemonitor-enabled: interval passes through")
    assert_scalars_clean(rendered, "servicemonitor-enabled")

    print("\nALL RENDER TESTS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
