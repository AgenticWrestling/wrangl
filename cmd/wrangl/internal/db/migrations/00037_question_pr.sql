-- +goose Up
-- A question can block a pull request: Smart Merge (resolve) runs ask the owner about conflicts
-- they cannot settle, and the Inbox nests those under the PR's review card. A side table, since
-- the question table's stored definition (widened in place by earlier migrations) is not safe to
-- ALTER.
CREATE TABLE question_pr (
    question_id INTEGER PRIMARY KEY REFERENCES question (id) ON DELETE CASCADE,
    pr_id       INTEGER NOT NULL REFERENCES pull_request (id) ON DELETE CASCADE
);
CREATE INDEX question_pr_pr ON question_pr (pr_id);

-- +goose Down
DROP TABLE question_pr;
