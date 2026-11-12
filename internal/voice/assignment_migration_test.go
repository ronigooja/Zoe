package voice

// rollbackVoiceAssignment applies when fixtures reconstruct the pre-217 schema.
const rollbackVoiceAssignment = rollbackVoiceLegacyCleanup + `DROP TABLE voice_context_registers;
DROP TABLE IF EXISTS voice_register_memory_boundaries;
DELETE FROM schema_migrations WHERE namespace='voice' AND version>=218;
ALTER TABLE voice_turns DROP COLUMN steward_action;
ALTER TABLE voice_turns DROP COLUMN steward_task_id;
DELETE FROM schema_migrations WHERE namespace='voice' AND version=217;`
