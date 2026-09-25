/*-------------------------------------------------------------------------
 *
 * pgcrypto_lite.c
 *	  The pgcrypto functions that need nothing but a strong random source.
 *
 * pgzero builds Postgres without OpenSSL, which pgcrypto requires. Its
 * gen_random_bytes() and gen_random_uuid() only need pg_strong_random()
 * (on pgzero, the host's secure random source), so this extension provides
 * them with pgcrypto's own code (contrib/pgcrypto/pgcrypto.c) and SQL
 * signatures.
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "fmgr.h"
#include "utils/fmgrprotos.h"
#include "varatt.h"

PG_MODULE_MAGIC_EXT(
					.name = "pgcrypto_lite",
					.version = PG_VERSION
);

/* SQL function: gen_random_bytes(int4) returns bytea */
PG_FUNCTION_INFO_V1(pg_random_bytes);

Datum
pg_random_bytes(PG_FUNCTION_ARGS)
{
	int			len = PG_GETARG_INT32(0);
	bytea	   *res;

	if (len < 1 || len > 1024)
		ereport(ERROR,
				(errcode(ERRCODE_EXTERNAL_ROUTINE_INVOCATION_EXCEPTION),
				 errmsg("Length not in range")));

	res = palloc(VARHDRSZ + len);
	SET_VARSIZE(res, VARHDRSZ + len);

	if (!pg_strong_random(VARDATA(res), len))
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("could not generate a random number")));

	PG_RETURN_BYTEA_P(res);
}

/* SQL function: gen_random_uuid() returns uuid */
PG_FUNCTION_INFO_V1(pg_random_uuid);

Datum
pg_random_uuid(PG_FUNCTION_ARGS)
{
	/* redirect to built-in function */
	return gen_random_uuid(fcinfo);
}
