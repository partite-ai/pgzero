/*
 * dl.c
 *	  dlopen() and friends over the table of statically linked modules.
 *
 * Linked with --wrap for each function (wasi-libc's libdl implements wasm
 * dynamic linking, which pgzero does not use).
 */
#include <dlfcn.h>
#include <stdio.h>
#include <stdbool.h>
#include <string.h>

#include "internal.h"

/* Programs without modules get an empty table; wasm/cmd/genmodules' wins. */
__attribute__((weak)) const pgzero_module pgzero_modules[] = {{0, 0}};

static char dl_error[256];
static bool dl_error_set;

static void
set_error(const char *fmt, const char *arg)
{
	snprintf(dl_error, sizeof(dl_error), fmt, arg);
	dl_error_set = true;
}

void *
__wrap_dlopen(const char *path, int mode)
{
	const char *base;
	size_t		len;

	if (path == NULL)
	{
		set_error("%s", "dlopen(NULL) is not supported");
		return NULL;
	}
	base = strrchr(path, '/');
	base = base ? base + 1 : path;
	len = strlen(base);
	/*
	 * Strip a shared library suffix. Besides our own ".so", clients may
	 * name modules with theirs (the regression tests use the suffix of
	 * the platform pg_regress was built for).
	 */
	for (const char *const *sfx = (const char *const[]) {".so", ".dylib", ".dll", NULL}; *sfx; sfx++)
	{
		size_t		n = strlen(*sfx);

		if (len > n && strcmp(base + len - n, *sfx) == 0)
		{
			len -= n;
			break;
		}
	}

	for (const pgzero_module *m = pgzero_modules; m->name; m++)
	{
		if (strlen(m->name) == len && strncmp(m->name, base, len) == 0)
			return (void *) m;
	}
	set_error("%s: module is not linked into this program", path);
	return NULL;
}

void *
__wrap_dlsym(void *handle, const char *name)
{
	const pgzero_module *m = handle;

	if (m == NULL || m == RTLD_DEFAULT)
	{
		set_error("%s: symbol lookup without a module handle is not supported", name);
		return NULL;
	}
	for (const pgzero_module_symbol *s = m->symbols; s->name; s++)
	{
		if (strcmp(s->name, name) == 0)
			return s->addr;
	}
	set_error("undefined symbol: %s", name);
	return NULL;
}

int
__wrap_dlclose(void *handle)
{
	return 0;
}

char *
__wrap_dlerror(void)
{
	if (!dl_error_set)
		return NULL;
	dl_error_set = false;
	return dl_error;
}
