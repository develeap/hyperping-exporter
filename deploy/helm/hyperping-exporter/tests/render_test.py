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

PyYAML is the only third-party dependency. helm must be on PATH.
Exit code 0 on success, 1 on any assertion failure.
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
EXPECTED_IMAGE_DEFAULT = "khaledsalhabdeveleap/hyperping-exporter:1.4.0"
EXPECTED_VERSION = "1.4.0"


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
    # Expect the label on Secret, Service, and Deployment (every resource
    # whose template includes the common-labels helper).
    expected_versions = {
        f"Secret/{r}": EXPECTED_VERSION for r in [d['metadata']['name'] for d in find_all(rendered, 'Secret')]
    }
    expected_versions.update({
        f"Service/{r}": EXPECTED_VERSION for r in [d['metadata']['name'] for d in find_all(rendered, 'Service')]
    })
    expected_versions.update({
        f"Deployment/{r}": EXPECTED_VERSION for r in [d['metadata']['name'] for d in find_all(rendered, 'Deployment')]
    })
    assert_eq(versions, expected_versions,
              "defaults: app.kubernetes.io/version label is 1.4.0 on every labelled resource")
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

    # Case 9 — mcpUrl with query string. `?`, `=`, `&` need no JSON
    # escaping; the URL passes through byte-for-byte.
    rendered = helm_template("mcp-url-query.values.yaml")
    assert_eq(deployment_args(rendered),
              BASELINE_ARGS + [
                  "--mcp-url=https://mcp.example.com/v1/mcp?team=core&strict=1",
              ],
              "mcp-url query: URL with query string passes through verbatim")
    assert_scalars_clean(rendered, "mcp-url query")

    print("\nALL RENDER TESTS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
