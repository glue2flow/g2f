-- API base URL of a connected account, if the provider names one (e.g. Salesforce instance_url).
ALTER TABLE connections ADD COLUMN instance_url text;
