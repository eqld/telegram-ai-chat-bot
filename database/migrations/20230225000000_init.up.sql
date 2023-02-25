CREATE TABLE IF NOT EXISTS chat_history (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL,
    username TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TEXT NOT NULL
);
