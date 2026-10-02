import { NextResponse, type NextRequest } from "next/server"

// Next 16 "proxy" (formerly middleware): optimistic check only. A request without a session
// cookie goes to the login page; whether the cookie is *valid* is decided by the scanner API
// (it answers 401, and the API client then sends the browser to /login).
const SESSION_COOKIE = "ds_session"

export function proxy(request: NextRequest) {
  const { pathname } = request.nextUrl
  if (pathname === "/login" || request.cookies.has(SESSION_COOKIE)) {
    return NextResponse.next()
  }
  return NextResponse.redirect(new URL("/login", request.url))
}

export const config = {
  matcher: ["/((?!api|_next/static|_next/image|favicon.ico|.*\\..*).*)"],
}
