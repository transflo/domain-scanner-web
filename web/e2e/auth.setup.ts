import { expect, test as setup } from "@playwright/test"

// Logs in once and keeps the session cookie for every other project.
setup("log in", async ({ page }) => {
  const password = process.env.E2E_PASSWORD
  if (!password) throw new Error("set E2E_PASSWORD to the admin password of the stack under test")
  await page.goto("/login")
  await page.getByLabel("口令").fill(password)
  await page.getByRole("button", { name: "登录" }).click()
  await expect(page.getByRole("heading", { name: "仪表盘" })).toBeVisible()
  await page.context().storageState({ path: "e2e/.auth/state.json" })
})
