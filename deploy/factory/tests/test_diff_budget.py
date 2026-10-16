"""MACH-58: the review diff excludes built/generated files, lists what it
skipped, and stays inside the model budget (chunk or summary mode)."""

from __future__ import annotations

import unittest
from unittest import mock

import deploy.factory.post_pr_review as post
from deploy.factory.review import diff_budget, profile as profile_mod
from deploy.factory.review.runner import CommandResult

MODEL_LIMIT_TOKENS = 262_144


def _file_diff(path: str, lines: int, width: int = 40) -> str:
    body = [f"diff --git a/{path} b/{path}", f"--- a/{path}", f"+++ b/{path}", f"@@ -1,1 +1,{lines} @@"]
    body += ["+" + "x" * width for _ in range(lines)]
    return "\n".join(body) + "\n"


class ParseTests(unittest.TestCase):
    def test_parse_diff_splits_files_and_reads_paths(self) -> None:
        text = _file_diff("src/a.py", 2) + _file_diff("web/dist/app.js", 2)
        files = diff_budget.parse_diff(text)
        self.assertEqual([file.path for file in files], ["src/a.py", "web/dist/app.js"])
        self.assertEqual(diff_budget.parse_diff(""), [])
        self.assertEqual(diff_budget.diff_path("diff --git a/src/a.py b/src/a.py"), "src/a.py")
        self.assertEqual(diff_budget.diff_path("not a header"), "")


class ExcludeTests(unittest.TestCase):
    def test_default_excludes_cover_the_spec_paths(self) -> None:
        for path in (
            "web/dist/index.js",
            "web/dist/assets/app.abc123.js",
            "src/app.min.js",
            "src/app.js.map",
            "package-lock.json",
            "yarn.lock",
            "Cargo.lock",
            "go.sum",
            "api/client.pb.go",
            "src/schema.generated.ts",
            "src/generated/types.py",
        ):
            self.assertTrue(
                diff_budget.path_excluded(path, diff_budget.DEFAULT_EXCLUDE_GLOBS), path
            )

    def test_normal_sources_are_kept(self) -> None:
        for path in ("src/app.py", "web/src/App.tsx", "README.md", "internal/skills/x.go"):
            self.assertFalse(
                diff_budget.path_excluded(path, diff_budget.DEFAULT_EXCLUDE_GLOBS), path
            )


    def test_unparsed_header_fails_closed(self) -> None:
        header = 'diff --git a/renamed "b/weird/na me".lock\n'
        self.assertEqual(diff_budget.diff_path(header), "")
        diff = header + "--- a/renamed\n+++ b/weird/na me\".lock\n+oops\n"
        plan = diff_budget.plan_diff(diff, ["*.lock"])
        self.assertEqual(plan.kept, [])
        self.assertEqual(plan.excluded, ["<unparsed diff header>"])
        self.assertEqual(plan.chunks[0].text, "")


class PlanTests(unittest.TestCase):
    def test_full_mode_when_the_filtered_diff_fits(self) -> None:
        diff = _file_diff("web/dist/app.js", 5000) + _file_diff("src/app.py", 20)
        plan = diff_budget.plan_diff(diff, diff_budget.DEFAULT_EXCLUDE_GLOBS, 200_000)
        self.assertEqual(plan.mode, diff_budget.MODE_FULL)
        self.assertEqual(plan.excluded, ["web/dist/app.js"])
        self.assertNotIn("web/dist/app.js", plan.prompt_text())
        self.assertIn("src/app.py", plan.prompt_text())
        notes = "\n".join(plan.notes())
        self.assertIn("web/dist/app.js", notes)

    def test_all_excluded_only_when_every_file_is_excluded(self) -> None:
        plan = diff_budget.plan_diff(_file_diff("web/dist/app.js", 5), diff_budget.DEFAULT_EXCLUDE_GLOBS, 200_000)
        self.assertTrue(any("every changed file was excluded" in note for note in plan.notes()))
        plan_empty = diff_budget.plan_diff("", diff_budget.DEFAULT_EXCLUDE_GLOBS, 200_000)
        self.assertFalse(any("every changed file was excluded" in note for note in plan_empty.notes()))

    def test_chunk_mode_shrinks_every_chunk_and_says_so(self) -> None:
        diff = "".join(_file_diff(f"src/f{index}.py", 200) for index in range(10))
        plan = diff_budget.plan_diff(diff, [], 8_000)
        self.assertEqual(plan.mode, diff_budget.MODE_CHUNK)
        self.assertGreater(len(plan.chunks), 1)
        for chunk in plan.chunks:
            self.assertLessEqual(diff_budget.estimate_tokens(chunk.text), 8_000)
        self.assertIn("chunked", "\n".join(plan.notes()))

    def test_summary_mode_for_a_single_oversized_file(self) -> None:
        plan = diff_budget.plan_diff(_file_diff("src/huge.py", 5000), [], 2_000)
        self.assertEqual(plan.mode, diff_budget.MODE_SUMMARY)
        self.assertEqual(plan.summarized, ["src/huge.py"])
        self.assertLessEqual(diff_budget.estimate_tokens(plan.prompt_text()), 2_000)
        self.assertIn("summary", "\n".join(plan.notes()))

    def test_env_overrides_the_default_budget(self) -> None:
        self.assertEqual(diff_budget.default_max_tokens({}), diff_budget.DEFAULT_MAX_TOKENS)
        self.assertEqual(diff_budget.default_max_tokens({"MACHINIST_REVIEW_DIFF_MAX_TOKENS": "1234"}), 1234)
        self.assertEqual(diff_budget.default_max_tokens({"MACHINIST_REVIEW_DIFF_MAX_TOKENS": "nonsense"}), diff_budget.DEFAULT_MAX_TOKENS)

    def test_summary_reports_the_plan_for_the_run_summary(self) -> None:
        plan = diff_budget.plan_diff(_file_diff("web/dist/app.js", 5) + _file_diff("src/a.py", 2), diff_budget.DEFAULT_EXCLUDE_GLOBS, 200_000)
        summary = plan.summary()
        self.assertEqual(summary["mode"], "full")
        self.assertEqual(summary["excluded"], ["web/dist/app.js"])
        self.assertEqual(summary["chunks"], 1)
        self.assertGreater(summary["tokens"]["unfiltered"], summary["tokens"]["filtered"])


class AcceptanceTests(unittest.TestCase):
    """PR #44 acceptance: a ~300k-token diff with a committed web/dist bundle
    must fit under the model limit, and the excluded files must be visible."""

    def _reviewer(self) -> dict:
        return {
            "name": "code-reviewer",
            "skills": ["feature-dev:code-reviewer"],
            "read_only": True,
        }

    def test_pr_44_shaped_bundle_stays_under_the_model_limit(self) -> None:
        # ~300k tokens of committed bundle plus a normal source change.
        bundle = _file_diff("web/dist/index.js", 250_000, width=60)
        source = _file_diff("src/app.py", 50)
        diff = bundle + source
        self.assertGreater(diff_budget.estimate_tokens(diff), MODEL_LIMIT_TOKENS)

        plan = diff_budget.plan_diff(diff, diff_budget.DEFAULT_EXCLUDE_GLOBS, diff_budget.DEFAULT_MAX_TOKENS)
        self.assertEqual(plan.excluded, ["web/dist/index.js"])

        captured: list[str] = []

        def fake_executor(args, prompt, timeout):
            captured.append(prompt)
            return CommandResult(tuple(args), 0, "NO FINDINGS", "")

        context = {
            "profile_name": "default",
            "repository": "owner/repo",
            "pr": "#44",
            "head_sha": "h",
            "base_sha": "b",
        }
        with mock.patch.object(post, "run_executor", side_effect=fake_executor):
            post.run_reviewer_passes([self._reviewer()], ["executor"], context, "", 5, 1, plan)

        self.assertTrue(captured)
        for prompt in captured:
            self.assertLess(diff_budget.estimate_tokens(prompt), MODEL_LIMIT_TOKENS)
            self.assertNotIn("web/dist/index.js\n@@", prompt)

    def test_huge_source_diff_is_chunked_and_every_prompt_fits(self) -> None:
        diff = "".join(_file_diff(f"src/module_{index}.py", 20_000, width=20) for index in range(4))
        plan = diff_budget.plan_diff(diff, diff_budget.DEFAULT_EXCLUDE_GLOBS, diff_budget.DEFAULT_MAX_TOKENS)
        self.assertEqual(plan.mode, diff_budget.MODE_CHUNK)

        captured: list[str] = []

        def fake_executor(args, prompt, timeout):
            captured.append(prompt)
            return CommandResult(tuple(args), 0, "NO FINDINGS", "")

        context = {"profile_name": "default", "repository": "owner/repo", "pr": "#44", "head_sha": "h", "base_sha": "b"}
        with mock.patch.object(post, "run_executor", side_effect=fake_executor):
            results = post.run_reviewer_passes([self._reviewer()], ["executor"], context, "", 5, 1, plan)

        self.assertEqual(len(results), len(plan.chunks))
        self.assertTrue(all("chunk" in result["name"] for result in results))
        for prompt in captured:
            self.assertLess(diff_budget.estimate_tokens(prompt), MODEL_LIMIT_TOKENS)


class DiffConfigTests(unittest.TestCase):
    def _resolved(self, body: dict) -> profile_mod.ResolvedProfile:
        return profile_mod.ResolvedProfile(body=body, raw="", source="test")

    def test_profile_excludes_replace_the_defaults(self) -> None:
        excludes, budget = post.diff_review_config(self._resolved({"diff": {"exclude": ["docs/**"], "max_tokens": 42}}), {})
        self.assertEqual(excludes, ["docs/**"])
        self.assertEqual(budget, 42)

    def test_explicit_empty_exclude_list_disables_all_exclusions(self) -> None:
        excludes, _budget = post.diff_review_config(self._resolved({"diff": {"exclude": []}}), {})
        self.assertEqual(excludes, [])

    def test_null_exclude_falls_back_to_the_defaults(self) -> None:
        excludes, _budget = post.diff_review_config(self._resolved({"diff": {"exclude": None}}), {})
        self.assertEqual(excludes, diff_budget.DEFAULT_EXCLUDE_GLOBS)

    def test_defaults_when_the_profile_omits_the_section(self) -> None:
        excludes, budget = post.diff_review_config(self._resolved({}), {})
        self.assertEqual(excludes, diff_budget.DEFAULT_EXCLUDE_GLOBS)
        self.assertEqual(budget, diff_budget.DEFAULT_MAX_TOKENS)

    def test_env_wins_over_the_profile(self) -> None:
        excludes, budget = post.diff_review_config(
            self._resolved({"diff": {"max_tokens": 42}}),
            {"MACHINIST_REVIEW_DIFF_MAX_TOKENS": "99"},
        )
        self.assertEqual(excludes, diff_budget.DEFAULT_EXCLUDE_GLOBS)
        self.assertEqual(budget, 99)


if __name__ == "__main__":
    unittest.main()