-- +goose Up
CREATE TABLE oauth_client (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  client_id TEXT NOT NULL,
  client_secret TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE oauth_client;
