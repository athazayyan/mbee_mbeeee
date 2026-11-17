-- The organizer's Instagram account for an event, stored without the leading @.
ALTER TABLE events ADD COLUMN instagram text CHECK (instagram IS NULL OR instagram ~ '^[A-Za-z0-9._]{1,30}$');
