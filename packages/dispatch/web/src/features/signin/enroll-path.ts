/** The path of the link a project's owner makes for a person from outside: `<outside
 *  origin>/enroll#<token>`, the token in the fragment so no server logs it and no browser sends it
 *  as a Referer. The app shows the enrollment page there in place of the sign-in gate, since the
 *  person has no session yet (`App`), and a link to it in stored words is the browser's to load
 *  (`useFollowInPlace`). */
export const ENROLL_PATH = "/enroll";
