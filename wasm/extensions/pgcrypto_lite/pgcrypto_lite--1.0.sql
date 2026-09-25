/* The pgcrypto functions that need only a strong random source, with
 * pgcrypto's definitions (contrib/pgcrypto/pgcrypto--1.3.sql). */

-- complain if script is sourced in psql, rather than via CREATE EXTENSION
\echo Use "CREATE EXTENSION pgcrypto_lite" to load this file. \quit

CREATE FUNCTION gen_random_bytes(int4)
RETURNS bytea
AS 'MODULE_PATHNAME', 'pg_random_bytes'
LANGUAGE C VOLATILE STRICT PARALLEL SAFE;

CREATE FUNCTION gen_random_uuid()
RETURNS uuid
AS 'MODULE_PATHNAME', 'pg_random_uuid'
LANGUAGE C VOLATILE PARALLEL SAFE;
