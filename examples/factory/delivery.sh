#!/usr/bin/env bash
set -euo pipefail

DRY_RUN=${FACTORY_DRY_RUN:-}
GIT_COMMAND=${FACTORY_GIT_COMMAND:-git}
GH_COMMAND=${FACTORY_GH_COMMAND:-gh}
CHROMIUM_COMMAND=${FACTORY_CHROMIUM_COMMAND:-chromium}
REVIEW_COMMAND=${FACTORY_REVIEW_COMMAND:-}
COMMENT_COMMAND=${FACTORY_COMMENT_COMMAND:-}
ISSUE_KEY=${FACTORY_ISSUE_KEY:-}
ISSUE_TITLE=${FACTORY_ISSUE_TITLE:-}
ISSUE_URL=${FACTORY_ISSUE_URL:-}
PROMPT_FILE=${FACTORY_PROMPT_FILE:-}
BRANCH=${FACTORY_BRANCH:-factory/${FACTORY_ISSUE_KEY:-}}
PR_URL=${FACTORY_PR_URL:-}
PREVIEW_URL=${FACTORY_PREVIEW_URL:-}
SCREENSHOT_DIR=${FACTORY_SCREENSHOT_DIR:-${MACHINIST_OUTPUT_DIR:-}}
SCREENSHOT_PREFIX=${FACTORY_SCREENSHOT_PREFIX:-${ISSUE_KEY}}
STEP=${1:-dry-run}

run() {
  if [[ -n "$DRY_RUN" ]]; then
    printf '[dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

require_issue() {
  [[ -n "$ISSUE_KEY" && -n "$ISSUE_TITLE" ]] || {
    echo 'FACTORY_ISSUE_KEY and FACTORY_ISSUE_TITLE are required' >&2
    return 1
  }
}

case "$STEP" in
  code)
    if [[ -n "$DRY_RUN" ]]; then
      printf '[dry-run] implement %s: %s\n' "${ISSUE_KEY:-ISSUE_KEY}" "${ISSUE_TITLE:-ISSUE_TITLE}"
    else
      require_issue
      printf '%s\n' "Implement $ISSUE_KEY: $ISSUE_TITLE"
    fi
    ;;
  commit)
    if [[ -n "$DRY_RUN" ]]; then
      run "$GIT_COMMAND" switch --create "${BRANCH:-factory/ISSUE_KEY}"
      run "$GIT_COMMAND" add --all
      run "$GIT_COMMAND" commit --message "Implement ${ISSUE_KEY:-ISSUE_KEY}: ${ISSUE_TITLE:-ISSUE_TITLE}"
      run "$GIT_COMMAND" push --set-upstream origin "${BRANCH:-factory/ISSUE_KEY}"
    else
      require_issue
      run "$GIT_COMMAND" switch --create "$BRANCH"
      run "$GIT_COMMAND" add --all
      run "$GIT_COMMAND" commit --message "Implement $ISSUE_KEY: $ISSUE_TITLE"
      run "$GIT_COMMAND" push --set-upstream origin "$BRANCH"
    fi
    ;;
  pull-request)
    if [[ -n "$DRY_RUN" ]]; then
      run "$GH_COMMAND" pr create --draft --title "${ISSUE_KEY:-ISSUE_KEY}: ${ISSUE_TITLE:-ISSUE_TITLE}" --body-file "${PROMPT_FILE:-PR_BODY_FILE}"
    else
      require_issue
      [[ -n "$PROMPT_FILE" ]] || { echo 'FACTORY_PROMPT_FILE is required' >&2; exit 2; }
      run "$GH_COMMAND" pr create --draft --title "$ISSUE_KEY: $ISSUE_TITLE" --body-file "$PROMPT_FILE"
    fi
    ;;
  review)
    if [[ -n "$DRY_RUN" ]]; then
      printf '[dry-run] adversarial review using %s\n' "${REVIEW_COMMAND:-REVIEW_COMMAND}"
    else
      [[ -n "$REVIEW_COMMAND" ]] || { echo 'FACTORY_REVIEW_COMMAND is required' >&2; exit 2; }
      "$REVIEW_COMMAND"
    fi
    ;;
  preview)
    if [[ -n "$DRY_RUN" ]]; then
      run mkdir --parents "${SCREENSHOT_DIR:-SCREENSHOT_DIR}"
      run "$CHROMIUM_COMMAND" --headless=new --no-sandbox --window-size=1280,960 --screenshot="${SCREENSHOT_DIR:-SCREENSHOT_DIR}/${SCREENSHOT_PREFIX:-SCREENSHOT_PREFIX}-desktop.png" "${PREVIEW_URL:-PREVIEW_URL}"
      run "$CHROMIUM_COMMAND" --headless=new --no-sandbox --window-size=420,960 --screenshot="${SCREENSHOT_DIR:-SCREENSHOT_DIR}/${SCREENSHOT_PREFIX:-SCREENSHOT_PREFIX}-mobile.png" "${PREVIEW_URL:-PREVIEW_URL}"
    else
      [[ -n "$PREVIEW_URL" ]] || { echo 'FACTORY_PREVIEW_URL is optional and was not set' >&2; exit 0; }
      [[ -n "$SCREENSHOT_DIR" ]] || { echo 'FACTORY_SCREENSHOT_DIR is required' >&2; exit 2; }
      run mkdir --parents "$SCREENSHOT_DIR"
      run "$CHROMIUM_COMMAND" --headless=new --no-sandbox --window-size=1280,960 --screenshot="$SCREENSHOT_DIR/$SCREENSHOT_PREFIX-desktop.png" "$PREVIEW_URL"
      run "$CHROMIUM_COMMAND" --headless=new --no-sandbox --window-size=420,960 --screenshot="$SCREENSHOT_DIR/$SCREENSHOT_PREFIX-mobile.png" "$PREVIEW_URL"
    fi
    ;;
  comment)
    if [[ -n "$DRY_RUN" ]]; then
      printf '[dry-run] comment on %s using %s\n' "${ISSUE_URL:-ISSUE_URL}" "${COMMENT_COMMAND:-COMMENT_COMMAND}"
    else
      [[ -n "$COMMENT_COMMAND" ]] || { echo 'FACTORY_COMMENT_COMMAND is required' >&2; exit 2; }
      [[ -n "$ISSUE_URL" ]] || { echo 'FACTORY_ISSUE_URL is required' >&2; exit 2; }
      FACTORY_ISSUE_URL="$ISSUE_URL" "$COMMENT_COMMAND"
    fi
    ;;
  dry-run)
    FACTORY_DRY_RUN=1 "$0" code
    FACTORY_DRY_RUN=1 "$0" commit
    FACTORY_DRY_RUN=1 "$0" pull-request
    FACTORY_DRY_RUN=1 "$0" review
    FACTORY_DRY_RUN=1 "$0" preview || true
    FACTORY_DRY_RUN=1 "$0" comment
    ;;
  *)
    echo "unknown factory step: $STEP" >&2
    exit 2
    ;;
esac
