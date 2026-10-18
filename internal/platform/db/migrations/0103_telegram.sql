-- +goose Up
-- Telegram as the reminders' second channel (decision Р-61 Б). A space's bot:
-- its token from @BotFather sealed with the encryption key, as a broker's
-- token is; its name; the last update read.
CREATE TABLE telegram_bots (
    space_id         uuid PRIMARY KEY REFERENCES spaces(id) ON DELETE CASCADE,
    token_ciphertext bytea NOT NULL,
    token_last4      text NOT NULL,
    username         text NOT NULL,
    update_offset    bigint NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- A member's chat with the space's bot, tied by a code the program gave them.
CREATE TABLE telegram_chats (
    user_id   uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    space_id  uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    chat_id   bigint NOT NULL,
    linked_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX telegram_chats_space ON telegram_chats (space_id);

-- The codes given to tie a chat, each for a quarter of an hour.
CREATE TABLE telegram_link_codes (
    code       text PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);

-- The kinds a member does not take in Telegram, and whether its messages
-- carry the sums — they do unless the member says otherwise.
ALTER TABLE push_prefs ADD COLUMN telegram_off text[] NOT NULL DEFAULT '{}',
    ADD COLUMN amounts boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE push_prefs DROP COLUMN amounts, DROP COLUMN telegram_off;
DROP TABLE telegram_link_codes;
DROP TABLE telegram_chats;
DROP TABLE telegram_bots;
