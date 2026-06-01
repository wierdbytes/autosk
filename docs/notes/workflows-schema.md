tasks:
- id
- author_id (fk:agents->id, nullable)
- title
- description
- workflow_id (fk:workflows->id, nullable)
- git_branch
- blocked_by
- status (enum: 'new', 'work', 'human', 'done', 'cancel')

agents:
- id
- name
- is_human (bool)

workflows:
- first_setp (fk:steps->id)
- description

workflow_origins:
- workflow_id (pk/fk:workflows->id, cascade delete)
- source_type
- source
- source_metadata (JSON text, nullable)
- definition_hash (canonical workflow definition SHA-256)
- revision
- active (0/1, default 1)
- created_at / updated_at (unix seconds)

steps:
- id
- agent_id (fk:agents->id)

steps_transitions:
- id
- step_id (fk:steps->id)
- next_step (fk:steps->id)
- prompt_rule

comments:
- id
- author (fk:agents->id)
- created_at
- task_id (fk:tasks->id)
- text
