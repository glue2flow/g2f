-- Executed once by the postgres image on first start: Keycloak gets its own database.
CREATE USER keycloak WITH PASSWORD 'keycloak';
CREATE DATABASE keycloak OWNER keycloak;
