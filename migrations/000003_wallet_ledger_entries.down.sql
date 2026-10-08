DROP TRIGGER wallet_ledger_consistency ON wallets;
DROP TABLE wallet_ledger_entries;
DROP FUNCTION wallet_ledger_consistency();
DROP FUNCTION ledger_transaction_processed();
DROP FUNCTION ledger_sequence();
DROP FUNCTION ledger_matches_transaction();
DROP FUNCTION ledger_append_only();
