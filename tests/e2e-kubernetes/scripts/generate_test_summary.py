#!/usr/bin/env python3
"""Prints a Markdown summary of a Ginkgo JSON report (`ginkgo --json-report`), for $GITHUB_STEP_SUMMARY.

Usage: generate_test_summary.py <test-report.json>
"""

import html
import json
import sys

FAILURE_STATES = {"failed", "aborted", "panicked", "interrupted", "timedout"}
# Keeps the summary well under GitHub's 1 MiB limit per step; the full report is uploaded as an artifact.
MAX_FAILURES = 50
MAX_MESSAGE_CHARS = 2000


def pre(text):
    if len(text) > MAX_MESSAGE_CHARS:
        text = text[:MAX_MESSAGE_CHARS] + "\n... (truncated, see the test report artifact)"
    # Test output goes in an escaped <pre> so it cannot inject Markdown or HTML into the summary.
    return f"<pre>\n{html.escape(text)}\n</pre>\n"


def spec_name(spec):
    texts = spec.get("ContainerHierarchyTexts") or []
    leaf = spec.get("LeafNodeText") or f"[{spec.get('LeafNodeType', '')}]"
    return " ".join(texts + [leaf])


def failure_block(spec):
    failure = spec.get("Failure") or {}
    location = failure.get("Location") or {}
    message = "\n".join(filter(None, [failure.get("Message"), failure.get("ForwardedPanic")]))
    return pre(f"[{spec['State']}] {spec_name(spec)}\n{location.get('FileName', '')}:{location.get('LineNumber', '')}\n\n{message}")


def summarize(path):
    try:
        with open(path) as f:
            suites = json.load(f)
    except FileNotFoundError:
        return f"### E2E test results\n\nNo test report at `{path}`: the tests did not run, did not finish, or wrote no report.\n"
    except (OSError, ValueError) as e:
        return f"### E2E test results\n\nCould not read the test report: {html.escape(str(e))}\n"

    specs = [s for suite in suites for s in suite.get("SpecReports") or []]
    its = [s for s in specs if s.get("LeafNodeType") == "It"]
    # Suite-level nodes (BeforeSuite, DeferCleanup, ...) are not counted, but are listed when they fail.
    failed = [s for s in specs if s.get("State") in FAILURE_STATES]
    passed = sum(s.get("State") == "passed" for s in its)
    failed_its = sum(s.get("State") in FAILURE_STATES for s in its)
    skipped = sum(s.get("State") in ("skipped", "pending") for s in its)
    # Failures outside any spec, such as a compile error or the suite timing out.
    reasons = [r for suite in suites for r in suite.get("SpecialSuiteFailureReasons") or []]

    out = [
        "### E2E test results\n",
        f"{passed} passed, {failed_its} failed, {skipped} skipped, {len(its)} specs in total.\n",
    ]
    # Ginkgo still writes a report, empty or partial, when the test process dies (os.Exit, OOM kill).
    if not (suites and all(s.get("SuiteSucceeded") for s in suites)) and not failed and not reasons:
        out.append("The suite failed without a spec failure in the report: see the job log.\n")
    if reasons:
        out.append("#### Suite failures\n")
        out += [pre(r) for r in reasons]
    # dict.fromkeys drops repeats: with -p, each process reports the same BeforeSuite failure.
    blocks = list(dict.fromkeys(failure_block(s) for s in failed))
    if blocks:
        out.append("#### Failed\n")
        out += blocks[:MAX_FAILURES]
        if len(blocks) > MAX_FAILURES:
            out.append(f"{len(blocks) - MAX_FAILURES} more failures are in the test report artifact.\n")
    return "\n".join(out)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    print(summarize(sys.argv[1]))
