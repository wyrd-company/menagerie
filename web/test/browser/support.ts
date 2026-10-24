import { expect, type APIRequestContext, type Page } from "@playwright/test";

export async function control(request: APIRequestContext, path: string, body: object = {}) {
  const response = await request.post(`/__fixture/${path}`, { data: body });
  expect(response.ok(), `fixture control ${path}`).toBeTruthy();
  return response;
}

export async function reset(request: APIRequestContext) {
  await control(request, "reset");
}

/** Saves a screenshot under evidence/screenshots for the handoff. */
export async function shot(page: Page, name: string) {
  await page.screenshot({ path: `evidence/screenshots/${name}.png`, fullPage: true });
}

/** Fails when the page scrolls sideways, the usual sign of a broken narrow layout. */
export async function expectNoHorizontalScroll(page: Page) {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, "horizontal overflow in pixels").toBeLessThanOrEqual(0);
}
