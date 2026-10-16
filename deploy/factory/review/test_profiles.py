"""Tests for review profiles (MACH-32): validation, precedence, lanes,
routing, reconcile and redaction. Run from deploy/factory:

    python3 -m unittest review.test_profiles
"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
import unittest.mock

import yaml

from . import core, profile as profile_mod, skill_library

REPO_ROOT = Path(__file__).resolve().parents[3]

VALID = """
name: test-profile
version: 1
checks:
  lanes:
    - lane: typescript
      when: ["**/*.ts"]
      commands:
        - run: "npx tsc --noEmit"
          if_exists: tsconfig.json
reviewers:
  - name: code-reviewer
    skills: ["feature-dev:code-reviewer"]
    parallel: true
    read_only: true
routing:
  fallback: [code-reviewer]
"""


def fake_git_show(paths: dict[str, str]):
    """run_git replacement returning canned content for `git show <sha>:<path>`."""

    def run_git(repo, args, timeout):
        if len(args) == 2 and args[0] == "show" and ":" in args[1]:
            _, ref_path = args
            content = paths.get(ref_path.split(":", 1)[1])
            if content is not None:
                return SimpleNamespace(ok=True, stdout=content, output="")
        return SimpleNamespace(ok=False, stdout="", output="nope")

    return run_git


class GlobTests(unittest.TestCase):
    def test_doublestar_matches_nested_and_root(self):
        self.assertTrue(profile_mod.glob_match("**/*.ts", "a/b/c.ts"))
        self.assertTrue(profile_mod.glob_match("**/*.ts", "x.ts"))
        self.assertFalse(profile_mod.glob_match("**/*.ts", "a/b/c.py"))

    def test_doublestar_trailing_requires_directory(self):
        self.assertTrue(profile_mod.glob_match("scripts/**", "scripts/nested/x.sh"))
        self.assertFalse(profile_mod.glob_match("scripts/**", "scripts"))

    def test_slashless_pattern_matches_basename_anywhere(self):
        self.assertTrue(profile_mod.glob_match("Cargo.toml", "Cargo.toml"))
        self.assertTrue(profile_mod.glob_match("Cargo.toml", "nested/Cargo.toml"))

    def test_single_star_does_not_cross_directories(self):
        self.assertFalse(profile_mod.glob_match("src/*.md", "src/posts/a.md"))
        self.assertTrue(profile_mod.glob_match("src/*.md", "src/a.md"))


class ValidationTests(unittest.TestCase):
    def parse(self, text: str):
        return profile_mod.parse_profile(text, "test")

    def test_valid_profile_parses(self):
        body = self.parse(VALID)
        self.assertEqual(body["name"], "test-profile")

    def test_frontmatter_markdown_accepted(self):
        body = self.parse("---\nname: md-profile\nversion: 1\n---\nbody")
        self.assertEqual(body["name"], "md-profile")

    def test_unknown_top_level_key_has_line_number(self):
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse(VALID + "\nwat: true\n")
        line, message = caught.exception.errors[0]
        self.assertIn("unknown top-level key: wat", message)
        self.assertGreater(line, 1)

    def test_bad_glob_is_rejected_with_line(self):
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse(VALID.replace('"**/*.ts"', "["))
        # A malformed glob is a YAML error at minimum; either way it fails.
        self.assertTrue(caught.exception.errors)

    def test_unknown_pass_reference_rejected(self):
        broken = VALID.replace("fallback: [code-reviewer]", "fallback: [ghost]")
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse(broken)
        self.assertTrue(any("ghost" in message for _, message in caught.exception.errors))

    def test_unknown_lane_reference_rejected(self):
        broken = VALID.replace("    read_only: true\n", "    read_only: true\n    lanes: [ghost]\n")
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse(broken)
        self.assertTrue(any("unknown lane: ghost" in message for _, message in caught.exception.errors))

    def test_reviewer_requires_skills_or_prompt(self):
        broken = VALID.replace('    skills: ["feature-dev:code-reviewer"]\n', "")
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse(broken)
        self.assertTrue(any("requires skills" in message for _, message in caught.exception.errors))


class PrecedenceTests(unittest.TestCase):
    def tearDown(self):
        profile_mod._control_plane_fetch = None

    def test_repo_base_ref_profile_wins(self):
        repo_profile = VALID.replace("name: test-profile", "name: repo-profile")
        resolved = profile_mod.resolve_profile(
            Path("."), "abc123", 10, fake_git_show({".machinist/review.yaml": repo_profile}), {}
        )
        self.assertEqual(resolved.source, "repo")
        self.assertEqual(resolved.name, "repo-profile")

    def test_control_plane_project_profile_used_without_repo_profile(self):
        profile_mod._control_plane_fetch = lambda env, timeout: {
            "profile": VALID.replace("name: test-profile", "name: project-profile"),
            "source": "project",
        }
        resolved = profile_mod.resolve_profile(Path("."), "abc123", 10, fake_git_show({}), {})
        self.assertEqual(resolved.source, "project")
        self.assertEqual(resolved.name, "project-profile")

    def test_global_default_when_project_unset(self):
        profile_mod._control_plane_fetch = lambda env, timeout: {
            "profile": VALID.replace("name: test-profile", "name: global-profile"),
            "source": "global",
        }
        resolved = profile_mod.resolve_profile(Path("."), "abc123", 10, fake_git_show({}), {})
        self.assertEqual(resolved.source, "global")

    def test_bundled_default_when_nothing_else(self):
        resolved = profile_mod.resolve_profile(Path("."), "abc123", 10, fake_git_show({}), {})
        self.assertEqual(resolved.source, "default")
        self.assertEqual(resolved.name, "default")

    def test_bundled_copy_matches_shipped_profile(self):
        shipped = (REPO_ROOT / "profiles" / "review" / "default.yaml").read_text(encoding="utf-8")
        flat = (Path(__file__).parent / "profiles" / "default.yaml").read_text(encoding="utf-8")
        self.assertEqual(shipped, flat)
        # And the shipped default carries no person-specific routing.
        self.assertNotIn("Daniel", shipped)
        self.assertNotIn("CLAUDE.md routing", shipped)


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.repo = Path(tempfile.mkdtemp(prefix="machinist-profile-test-"))
        (self.repo / "tsconfig.json").write_text("{}\n")

    def test_lane_matches_changed_files(self):
        body = self._profile([{"run": "npx tsc --noEmit", "if_exists": "tsconfig.json"}], when=["**/*.ts"])
        checks, _ = profile_mod.plan_checks(
            self.repo, body, ["src/a.ts"], core.classify_changed_files(["src/a.ts"]), []
        )
        self.assertEqual([check.name for check in checks], ["npx tsc --noEmit"])
        self.assertEqual(checks[0].args, ["npx", "tsc", "--noEmit"])

    def test_lane_skipped_when_no_files_match(self):
        body = self._profile([{"run": "npx tsc --noEmit", "if_exists": "tsconfig.json"}], when=["**/*.ts"])
        checks, _ = profile_mod.plan_checks(
            self.repo, body, ["main.py"], core.classify_changed_files(["main.py"]), []
        )
        self.assertEqual(checks, [])

    def test_if_exists_falls_back_to_else(self):  # noqa: E303
        body = self._profile([{"run": "bin/ci", "if_exists": "bin/ci", "else": ["bundle exec rubocop"]}])
        checks, _ = profile_mod.plan_checks(
            self.repo, body, ["a.rb"], core.classify_changed_files(["a.rb"]), []
        )
        self.assertEqual([check.args for check in checks], [["bundle", "exec", "rubocop"]])

    def test_missing_tool_blocked_by_default(self):
        body = self._profile(["definitely-not-a-tool-xyz check {changed}"])
        checks, _ = profile_mod.plan_checks(
            self.repo, body, ["a.py"], core.classify_changed_files(["a.py"]), []
        )
        self.assertEqual(checks[0].classification, "blocked")

    def test_missing_tool_skip_policy(self):
        body = self._profile(["definitely-not-a-tool-xyz check"], missing_tool="skip")
        checks, notes = profile_mod.plan_checks(
            self.repo, body, ["a.py"], core.classify_changed_files(["a.py"]), []
        )
        self.assertEqual(checks, [])
        self.assertTrue(any("definitely-not-a-tool-xyz" in note for note in notes))

    def test_changed_placeholder_scopes_to_lane_files(self):
        body = self._profile(["echo {changed}"], lane="python", when=["**/*.py"])
        checks, _ = profile_mod.plan_checks(
            self.repo,
            body,
            ["a.py", "b.py"],
            core.classify_changed_files(["a.py", "b.py"]),
            [],
        )
        self.assertEqual(checks[0].args, ["echo", "a.py", "b.py"])

    def test_changed_py_placeholder_uses_language_detection(self):
        body = self._profile(["echo {changed_py}"], lane="omarchy", when=["**/*.sh"])
        checks, _ = profile_mod.plan_checks(
            self.repo,
            body,
            ["x.sh", "y.py"],
            core.classify_changed_files(["x.sh", "y.py"]),
            [],
        )
        self.assertEqual(checks[0].args, ["echo", "y.py"])

    def test_if_script_requires_package_json_script(self):
        body = self._profile(["npm run lint", {"run": "npm test", "if_script": "test"}])
        checks, notes = profile_mod.plan_checks(
            self.repo, body, ["a.ts"], core.classify_changed_files(["a.ts"]), []
        )
        # No package.json in the temp repo: first command unguarded runs,
        # the if_script one is skipped with a note.
        self.assertEqual([check.name for check in checks], ["npm run lint"])
        self.assertTrue(any("npm test" in note for note in notes))

    def _profile(self, commands, lane="typescript", when=None, missing_tool="blocked"):
        when = when or ["**/*.ts", "**/*.py", "**/*.rb", "**/*.sh"]
        body = {
            "name": "plan-test",
            "version": 1,
            "checks": {
                "blocking": "new_failures",
                "missing_tool": missing_tool,
                "lanes": [
                    {
                        "lane": lane,
                        "when": when,
                        "commands": [yaml.safe_load(command) if isinstance(command, str) else command for command in commands],
                    }
                ],
            },
            "reviewers": [{"name": "code-reviewer", "skills": ["feature-dev:code-reviewer"]}],
        }
        return profile_mod.parse_profile(yaml.safe_dump(body), "plan-test")


class SelectionTests(unittest.TestCase):
    def setUp(self):
        self.body = profile_mod.parse_profile(
            (REPO_ROOT / "profiles" / "review" / "dan-pr-review.yaml").read_text(encoding="utf-8"),
            "dan-pr-review",
        )

    def test_prose_routing_adds_prose_pass(self):
        selected = profile_mod.select_reviewers(self.body, ["src/content/a.md"], set(), True, False)
        self.assertIn("prose", [reviewer["name"] for reviewer in selected])

    def test_lane_reviewer_added_for_lane_match(self):
        selected = profile_mod.select_reviewers(self.body, ["a.ts"], {"typescript"}, True, False)
        self.assertIn("ts-review", [reviewer["name"] for reviewer in selected])

    def test_unreadable_files_fall_back(self):
        selected = profile_mod.select_reviewers(self.body, [], set(), False, False)
        self.assertEqual(
            sorted(reviewer["name"] for reviewer in selected),
            ["code-reviewer", "secret-scan", "silent-failure-hunter", "test-analyzer"],
        )

    def test_adversarial_only_on_request(self):
        base = profile_mod.select_reviewers(self.body, ["main.go"], set(), True, False)
        self.assertNotIn("adversarial", [reviewer["name"] for reviewer in base])
        flagged = profile_mod.select_reviewers(self.body, ["main.go"], set(), True, True)
        self.assertIn("adversarial", [reviewer["name"] for reviewer in flagged])

    def test_routing_only_pass_excluded_without_routing_match(self):
        selected = profile_mod.select_reviewers(self.body, ["main.go"], set(), True, False)
        self.assertNotIn("prose", [reviewer["name"] for reviewer in selected])


class ReconcileTests(unittest.TestCase):
    def test_dedupe_and_verify_against_changed_files(self):
        reviewers = [
            {"name": "a", "findings": [{"severity": "bug", "location": "x.py:1", "message": "m"}]},
            {"name": "b", "findings": [{"severity": "bug", "location": "x.py:1", "message": "m"}, {"severity": "nit", "location": "elsewhere.py:9", "message": "n"}]},
        ]
        merged = profile_mod.reconcile_findings(
            reviewers, {"bug"}, ["x.py"], dedupe=True, verify_against_code=True
        )
        self.assertEqual(len(merged), 2)
        by_message = {finding["message"]: finding for finding in merged}
        # x.py is in the changed-file list -> verified; elsewhere.py is not.
        self.assertTrue(by_message["m"].get("verified"))
        self.assertFalse(by_message["n"].get("verified"))
        blocking = profile_mod.blocking_findings(merged)
        self.assertEqual(len(blocking), 1)
        self.assertEqual(blocking[0]["message"], "m")


class SkillLibraryTests(unittest.TestCase):
    def test_shipped_skill_resolves(self):
        resolved = skill_library.resolve("feature-dev:code-reviewer")
        self.assertIsNotNone(resolved)
        self.assertIn("review", resolved[0].lower())

    def test_unknown_skill_unavailable(self):
        self.assertIsNone(skill_library.resolve("nope:nothing"))

    def test_prompt_alias_resolves(self):
        self.assertIsNotNone(skill_library.resolve("docs-reviewer"))

    def test_versioned_library_active_version_wins(self):
        with tempfile.TemporaryDirectory() as data_dir:
            version_dir = Path(data_dir) / "skills" / "feature-dev_code-reviewer" / "abc123"
            version_dir.mkdir(parents=True)
            (version_dir / "SKILL.md").write_text(
                "---\nname: feature-dev:code-reviewer\ndescription: library copy\n---\nLIBRARY REVIEW\n",
                encoding="utf-8",
            )
            (Path(data_dir) / "skills" / "feature-dev_code-reviewer" / "manifest.json").write_text(
                json.dumps({"name": "feature-dev:code-reviewer", "active": "abc123", "versions": [{"version": "abc123"}]}),
                encoding="utf-8",
            )
            with unittest.mock.patch.dict("os.environ", {"MACHINIST_DATA_DIR": data_dir}):
                resolved = skill_library.resolve("feature-dev:code-reviewer")
        self.assertIsNotNone(resolved)
        self.assertEqual(resolved[1], "abc123")
        self.assertIn("LIBRARY REVIEW", resolved[0])

    def test_library_without_active_version_falls_back(self):
        with tempfile.TemporaryDirectory() as data_dir:
            (Path(data_dir) / "skills" / "feature-dev_code-reviewer").mkdir(parents=True)
            (Path(data_dir) / "skills" / "feature-dev_code-reviewer" / "manifest.json").write_text(
                json.dumps({"name": "feature-dev:code-reviewer", "active": ""}),
                encoding="utf-8",
            )
            with unittest.mock.patch.dict("os.environ", {"MACHINIST_DATA_DIR": data_dir}):
                resolved = skill_library.resolve("feature-dev:code-reviewer")
        self.assertIsNotNone(resolved)
        self.assertIn("review", resolved[0].lower())


class RedactionTests(unittest.TestCase):
    def test_tokens_masked(self):
        text = "token ghp_0123456789abcdefghij0123456789 and Authorization: Bearer abc.def and api_key = supersecret123"
        redacted = core.redact(text)
        self.assertNotIn("ghp_0123456789abcdefghij0123456789", redacted)
        self.assertNotIn("abc.def", redacted)
        self.assertNotIn("supersecret123", redacted)
        self.assertIn("[REDACTED]", redacted)

    def test_plain_output_untouched(self):
        self.assertEqual(core.redact("npm ERR! missing script: lint"), "npm ERR! missing script: lint")


class AmendBudgetTests(unittest.TestCase):
    def _profile(self, rounds):
        body = profile_mod.parse_profile(VALID, "test")
        body["blocking"] = {"amend_rounds": rounds}
        return profile_mod.ResolvedProfile(body=body, raw=VALID, source="test")

    def test_profile_value_used_by_default(self):
        self.assertEqual(profile_mod.amend_rounds_budget(self._profile(5), None, {}), 5)

    def test_env_overrides_profile(self):
        self.assertEqual(profile_mod.amend_rounds_budget(self._profile(5), None, {"MACHINIST_REVIEW_AMEND_ROUNDS": "1"}), 1)

    def test_cli_overrides_env(self):
        self.assertEqual(profile_mod.amend_rounds_budget(self._profile(5), 2, {"MACHINIST_REVIEW_AMEND_ROUNDS": "1"}), 2)


class ReviewFixRegressionTests(unittest.TestCase):
    def parse(self, text: str):
        return profile_mod.parse_profile(text, "test")

    def test_missing_tool_policy_applies_to_ecosystem_runners(self):
        # npm/cargo/go are not assumed installed: with nothing on PATH the
        # missing_tool policy applies instead of a silent 127 "pre-existing".
        body = self.parse(VALID)
        body["checks"]["lanes"][0]["commands"] = ["npm", "run", "lint"]
        with unittest.mock.patch("shutil.which", return_value=None):
            checks, _ = profile_mod.plan_checks(
                Path("."), body, ["a.ts"], core.classify_changed_files(["a.ts"]), []
            )
        self.assertTrue(checks)
        self.assertTrue(all(check.classification == "blocked" for check in checks))

    def test_extends_inherits_base_reviewers(self):
        body = self.parse("name: child\nversion: 1\nextends: default\n")
        self.assertEqual(body["name"], "child")
        names = [reviewer["name"] for reviewer in body["reviewers"]]
        self.assertIn("code-reviewer", names)
        self.assertIn("silent-failure-hunter", names)

    def test_extends_routing_may_reference_base_reviewers(self):
        # Before extends support this failed validation with "unknown pass".
        body = self.parse("name: child\nversion: 1\nextends: default\nrouting:\n  fallback: [silent-failure-hunter]\n")
        self.assertEqual(body["routing"]["fallback"], ["silent-failure-hunter"])

    def test_extends_unknown_profile_rejected(self):
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse("name: child\nversion: 1\nextends: no-such-profile\n")
        self.assertIn("extends references unknown profile", caught.exception.errors[0][1])

    def test_unreadable_files_without_fallback_runs_ordinary_reviewers(self):
        body = self.parse(VALID)
        body["reviewers"].append({"name": "prose", "skills": ["write-as-dan"], "routing_only": True})
        body["routing"] = {"rules": []}
        selected = profile_mod.select_reviewers(body, [], set(), False, False)
        names = [reviewer["name"] for reviewer in selected]
        self.assertIn("code-reviewer", names)
        self.assertNotIn("prose", names)

    def test_list_form_checks_policy_values_validated(self):
        text = """
name: list-checks
version: 1
checks:
  - blocking: sometimes
  - lane: typescript
    when: ["**/*.ts"]
    commands:
      - run: "npx tsc --noEmit"
reviewers:
  - name: code-reviewer
    skills: ["feature-dev:code-reviewer"]
routing:
  fallback: [code-reviewer]
"""
        with self.assertRaises(profile_mod.ProfileError) as caught:
            self.parse(text)
        self.assertTrue(any("checks.blocking" in message for _, message in caught.exception.errors))


class TruffleHogTests(unittest.TestCase):
    """MACH-41: the built-in review scans the PR range with TruffleHog. A
    verified credential is blocking; unverified/unknown are reported only, and
    secret values are never echoed (only TruffleHog's redacted form)."""

    FIXTURE = REPO_ROOT / "deploy" / "factory" / "tests" / "fixtures" / "trufflehog.jsonl"

    def payloads(self):
        return core.parse_trufflehog_output(self.FIXTURE.read_text(encoding="utf-8"))

    def test_scan_args_cover_the_pr_range(self):
        args = core.trufflehog_scan_args("/repo", "base123", "feature", "verified")
        self.assertEqual(args[:2], ["trufflehog", "git"])
        self.assertIn("file:///repo", args)
        self.assertIn("--since-commit=base123", args)
        self.assertIn("--branch=feature", args)
        self.assertIn("--results=verified", args)
        self.assertIn("--json", args)

    def test_parse_skips_non_json_noise(self):
        log_line = '{"level":"info-0","ts":"2026-01-01","logger":"trufflehog","msg":"running source"}'
        text = "\U0001f437 banner\n" + log_line + "\n" + self.FIXTURE.read_text(encoding="utf-8") + "not json\n"
        self.assertEqual(len(core.parse_trufflehog_output(text)), 2)

    def test_verified_finding_blocks_and_redacts(self):
        payloads = self.payloads()
        finding = core.trufflehog_finding(payloads[0], True)
        self.assertTrue(finding["blocking"])
        self.assertEqual(finding["severity"], "critical")
        self.assertEqual(finding["location"], "src/config.py:12")
        self.assertIn("AKIA****", finding["message"])
        # The raw (base64) secret and its decoded form are never surfaced.
        self.assertNotIn("cGxhY2Vob2xkZXI=", finding["message"])
        self.assertNotIn("placeholder", finding["message"])

    def test_unverified_finding_does_not_block(self):
        payloads = self.payloads()
        finding = core.trufflehog_finding(payloads[1], False)
        self.assertFalse(finding["blocking"])
        self.assertEqual(finding["severity"], "warning")
        self.assertEqual(finding["location"], "docs/example.md:3")

    def test_location_and_redaction_fall_back(self):
        self.assertEqual(core.trufflehog_location({}), "(unknown)")
        self.assertIn("[REDACTED]", core.trufflehog_finding({"DetectorName": "X"}, True)["message"])

    def test_bundled_skill_resolves(self):
        self.assertIsNotNone(skill_library.resolve("trufflehog"))

    def test_shipped_profiles_list_the_secret_pass(self):
        for name in ("default", "dan-pr-review"):
            body = profile_mod.parse_profile(
                (REPO_ROOT / "profiles" / "review" / f"{name}.yaml").read_text(encoding="utf-8"), name
            )
            reviewers = {reviewer["name"]: reviewer for reviewer in body["reviewers"]}
            self.assertIn("trufflehog", reviewers["secret-scan"]["skills"])


if __name__ == "__main__":
    unittest.main()
