CREATE TABLE secrets (
    name        TEXT    NOT NULL PRIMARY KEY,
    ciphertext  BLOB    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;
