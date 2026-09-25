/*
 * shell.c
 *	  system() and popen() without a shell.
 *
 * Postgres's tools run each other through the shell: initdb starts
 * `"postgres" --boot ... ` with popen(), probes settings with
 * `"postgres" --check ... < "/dev/null" > "/dev/null" 2>&1` through
 * system(), and find_other_exec() reads `"postgres" -V` with popen().
 * This implements the subset of sh those command lines use - quoted words,
 * and redirection of stdin/stdout/stderr to /dev/null or to each other -
 * directly on pgzero_spawn(). Anything else (pipelines, variables, ...)
 * fails with ENOSYS.
 */
#include <ctype.h>
#include <stdbool.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#include "internal.h"

#define MAX_ARGS 256

typedef struct command
{
	char	   *argv[MAX_ARGS + 1];
	int			argc;
	/* For each of fds 0-2: -1 (unchanged), or the fd it becomes a copy of. */
	int			redirect[3];
	char	   *storage;
} command;

static void
free_command(command *cmd)
{
	free(cmd->storage);
}

/*
 * Parse a command line. Words go into cmd->storage, a buffer as long as
 * the line (words are never longer than their source text).
 */
static int
parse_command(const char *line, command *cmd, int *devnull)
{
	char	   *out;
	const char *p = line;

	memset(cmd, 0, sizeof(*cmd));
	cmd->redirect[0] = cmd->redirect[1] = cmd->redirect[2] = -1;
	cmd->storage = out = malloc(strlen(line) + 1);
	if (!out)
		return -1;

	for (;;)
	{
		int			redir_fd = -1;
		bool		redir_in = false;
		char	   *word;
		bool		have_word = false;

		while (isspace((unsigned char) *p))
			p++;
		if (*p == '\0')
			break;

		/* Redirection operators: [n]> [n]< [n]>&m */
		if ((p[0] == '>' || p[0] == '<') ||
			(isdigit((unsigned char) p[0]) && (p[1] == '>' || p[1] == '<')))
		{
			if (isdigit((unsigned char) p[0]))
				redir_fd = *p++ - '0';
			redir_in = (*p == '<');
			if (redir_fd < 0)
				redir_fd = redir_in ? 0 : 1;
			p++;
			if (*p == '>')		/* >> appends; same for /dev/null */
				p++;
			if (*p == '&' && isdigit((unsigned char) p[1]))
			{
				int			src = p[1] - '0';

				p += 2;
				if (redir_fd > 2 || src > 2)
					goto unsupported;
				cmd->redirect[redir_fd] = src;
				continue;
			}
			while (isspace((unsigned char) *p))
				p++;
		}

		/* A word, with quoting. */
		word = out;
		while (*p && !isspace((unsigned char) *p))
		{
			if (*p == '\'')
			{
				const char *end = strchr(p + 1, '\'');

				if (!end)
					goto unsupported;
				memcpy(out, p + 1, end - p - 1);
				out += end - p - 1;
				p = end + 1;
			}
			else if (*p == '"')
			{
				p++;
				while (*p && *p != '"')
				{
					if (*p == '\\' && strchr("\"\\$`", p[1]))
						p++;
					else if (*p == '$' || *p == '`')
						goto unsupported;
					*out++ = *p++;
				}
				if (*p != '"')
					goto unsupported;
				p++;
			}
			else if (*p == '\\' && p[1])
			{
				*out++ = p[1];
				p += 2;
			}
			else if (strchr("|&;()<>$`*?[", *p))
				goto unsupported;
			else
				*out++ = *p++;
			have_word = true;
		}
		*out++ = '\0';
		if (!have_word)
			goto unsupported;

		if (redir_fd >= 0)
		{
			/* Only /dev/null can be a redirection target. */
			if (redir_fd > 2 || strcmp(word, "/dev/null") != 0)
				goto unsupported;
			if (*devnull < 0 && (*devnull = host_devnull()) < 0)
			{
				errno = -*devnull;
				free_command(cmd);
				return -1;
			}
			cmd->redirect[redir_fd] = -2;	/* /dev/null */
			(void) redir_in;
			continue;
		}

		if (cmd->argc == 0 && strcmp(word, "exec") == 0)
			continue;
		if (cmd->argc >= MAX_ARGS)
			goto unsupported;
		cmd->argv[cmd->argc++] = word;
	}
	if (cmd->argc == 0)
		goto unsupported;
	cmd->argv[cmd->argc] = NULL;
	return 0;

unsupported:
	free_command(cmd);
	errno = ENOSYS;
	return -1;
}

/*
 * Spawn cmd with its redirections applied. extra_fd/extra_target
 * additionally make fd extra_fd (0 or 1) a copy of extra_target, for
 * popen(). Our own stdio is saved and restored around the spawn.
 */
static pid_t
spawn_command(command *cmd, int devnull, int extra_fd, int extra_target)
{
	int			saved[3];
	int			target[3];
	pid_t		pid;
	int			save_errno;

	for (int fd = 0; fd < 3; fd++)
		target[fd] = -1;
	if (extra_fd >= 0)
		target[extra_fd] = extra_target;
	for (int fd = 0; fd < 3; fd++)
	{
		if (cmd->redirect[fd] == -2)
			target[fd] = devnull;
		else if (cmd->redirect[fd] >= 0)
			target[fd] = target[cmd->redirect[fd]] >= 0 ? target[cmd->redirect[fd]] : cmd->redirect[fd];
	}

	for (int fd = 0; fd < 3; fd++)
	{
		saved[fd] = -1;
		if (target[fd] >= 0 && target[fd] != fd)
		{
			saved[fd] = dup(fd);
			dup2(target[fd], fd);
		}
	}
	pid = pgzero_spawn(cmd->argv);
	save_errno = errno;
	for (int fd = 0; fd < 3; fd++)
	{
		if (saved[fd] >= 0)
		{
			dup2(saved[fd], fd);
			close(saved[fd]);
		}
	}
	errno = save_errno;
	return pid;
}

int
system(const char *line)
{
	command		cmd;
	int			devnull = -1;
	pid_t		pid;
	int			status;

	if (line == NULL)
		return 1;				/* a command processor is available */
	if (parse_command(line, &cmd, &devnull) < 0)
		return errno == ENOSYS ? 127 << 8 : -1;
	fflush(NULL);
	pid = spawn_command(&cmd, devnull, -1, -1);
	free_command(&cmd);
	if (devnull >= 0)
		close(devnull);
	if (pid < 0)
		return 127 << 8;
	while (waitpid(pid, &status, 0) < 0)
	{
		if (errno != EINTR)
			return -1;
	}
	return status;
}

/* Child pids of streams opened with popen(). */
typedef struct popen_entry
{
	FILE	   *stream;
	pid_t		pid;
	struct popen_entry *next;
} popen_entry;

static popen_entry *popen_list;

FILE *
popen(const char *line, const char *mode)
{
	command		cmd;
	int			devnull = -1;
	int			fds[2];
	bool		writing;
	int			ours,
				theirs;
	pid_t		pid;
	FILE	   *stream;
	popen_entry *entry;

	if ((mode[0] != 'r' && mode[0] != 'w') || (mode[1] != '\0' && mode[1] != 'e'))
	{
		errno = EINVAL;
		return NULL;
	}
	writing = mode[0] == 'w';
	if (parse_command(line, &cmd, &devnull) < 0)
		return NULL;
	entry = malloc(sizeof(*entry));
	if (!entry || pipe(fds) < 0)
	{
		free(entry);
		free_command(&cmd);
		if (devnull >= 0)
			close(devnull);
		return NULL;
	}
	ours = writing ? fds[1] : fds[0];
	theirs = writing ? fds[0] : fds[1];
	/* The child must not hold our end, or it would never see EOF. */
	fcntl(ours, F_SETFD, FD_CLOEXEC);

	fflush(NULL);
	pid = spawn_command(&cmd, devnull, writing ? 0 : 1, theirs);
	free_command(&cmd);
	close(theirs);
	if (devnull >= 0)
		close(devnull);
	if (pid < 0)
	{
		close(ours);
		free(entry);
		return NULL;
	}
	stream = fdopen(ours, writing ? "w" : "r");
	if (!stream)
	{
		close(ours);
		free(entry);
		return NULL;
	}
	entry->stream = stream;
	entry->pid = pid;
	entry->next = popen_list;
	popen_list = entry;
	return stream;
}

int
pclose(FILE *stream)
{
	popen_entry **pp;
	popen_entry *entry;
	int			status;

	for (pp = &popen_list; *pp && (*pp)->stream != stream; pp = &(*pp)->next)
		;
	if (!*pp)
	{
		errno = ECHILD;
		return -1;
	}
	entry = *pp;
	*pp = entry->next;
	fclose(stream);
	while (waitpid(entry->pid, &status, 0) < 0)
	{
		if (errno != EINTR)
		{
			free(entry);
			return -1;
		}
	}
	free(entry);
	return status;
}
