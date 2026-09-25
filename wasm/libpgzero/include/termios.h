/* pgzero: there are no terminals; the functions fail with ENOTTY. */
#ifndef PGZERO_TERMIOS_H
#define PGZERO_TERMIOS_H
#include <sys/types.h>
#ifdef __cplusplus
extern "C" {
#endif
typedef unsigned int tcflag_t;
typedef unsigned char cc_t;
typedef unsigned int speed_t;
#define NCCS 32
struct termios
{
	tcflag_t	c_iflag;
	tcflag_t	c_oflag;
	tcflag_t	c_cflag;
	tcflag_t	c_lflag;
	cc_t		c_line;
	cc_t		c_cc[NCCS];
};
#define ECHO   0000010
#define ICANON 0000002
#define ECHONL 0000100
#define TCSANOW   0
#define TCSADRAIN 1
#define TCSAFLUSH 2
int			tcgetattr(int, struct termios *);
int			tcsetattr(int, int, const struct termios *);
#ifdef __cplusplus
}
#endif
#endif
