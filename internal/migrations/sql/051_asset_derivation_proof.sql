-- Historical completion rows have no structured proof. New proof is written
-- in the same transaction as the completed output and its upload session.
ALTER TABLE assets ADD COLUMN derivation_proof jsonb;
