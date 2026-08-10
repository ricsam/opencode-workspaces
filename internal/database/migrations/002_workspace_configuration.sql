CREATE TABLE IF NOT EXISTS workspace_configurations (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    opencode_json_ciphertext text NOT NULL,
    api_key_ciphertext text NOT NULL,
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS oidc_identities_issuer_user_unique
    ON oidc_identities (issuer, user_id);
