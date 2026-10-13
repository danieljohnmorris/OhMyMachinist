// Scoped API tokens (MACH-36): created and revoked from Settings. The
// plaintext secret is returned once at creation and never stored in the
// browser; only the SHA-256 hash lives in the control plane database.

function tokensFromPayload(payload) {
  return Array.isArray(payload?.tokens)
    ? payload.tokens.map((token) => ({
        id: token.id,
        name: token.name,
        scope: token.scope,
        createdAt: token.created_at,
        revokedAt: token.revoked_at || null,
      }))
    : [];
}

export async function loadTokens(csrfToken, signal) {
  const response = await fetch("/api/v1/tokens", {
    headers: { "X-Machinist-CSRF": csrfToken },
    signal,
  });
  if (!response.ok) throw new Error(`Token list request failed (${response.status})`);
  return tokensFromPayload(await response.json());
}

export async function createToken(csrfToken, { name, scope }) {
  const response = await fetch("/api/v1/tokens", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ name, scope }),
  });
  if (!response.ok) throw new Error(`Token create failed (${response.status})`);
  const payload = await response.json();
  return { token: tokensFromPayload({ tokens: [payload.token] })[0], secret: payload.secret };
}

export async function revokeToken(csrfToken, id) {
  const response = await fetch(`/api/v1/tokens/${encodeURIComponent(id)}`, {
    method: "DELETE",
    headers: { "X-Machinist-CSRF": csrfToken },
  });
  if (!response.ok && response.status !== 404) {
    throw new Error(`Token revoke failed (${response.status})`);
  }
}
