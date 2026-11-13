// DirectInput FF check for a wheel in a VR pod (XERK-2849): waits up to 30s for
// a game controller, lists it, plays constant/spring/damper effects (all alive
// at once, as a racing game holds them) and prints live axis state.
//
// Build: x86_64-w64-mingw32-gcc -O2 -o fftest.exe fftest.c -ldinput8 -ldxguid -luser32 -lole32
// Run (in the pod, with an X display): copy to /work, then
//   STEAM_COMPAT_DATA_PATH=<writable dir> STEAM_COMPAT_CLIENT_INSTALL_PATH=<steam dir>
//   <SteamLinuxRuntime_sniper>/_v2-entry-point --verb=waitforexitandrun --
//   <writable copy of Proton 9.0>/proton waitforexitandrun /work/fftest.exe
// Output goes to /work/fftest.log (Proton drops the exe's stdout).
#define DIRECTINPUT_VERSION 0x0800
#define INITGUID
#include <windows.h>
#include <dinput.h>
#include <stdio.h>

static IDirectInput8W *di;
static GUID first; static int found;

static BOOL CALLBACK enumDev(const DIDEVICEINSTANCEW *d, void *ctx) {
	printf("device: %ls product=%ls guidProduct=%08lx type=%#lx ff=%d\n", d->tszInstanceName,
		d->tszProductName, d->guidProduct.Data1, d->dwDevType, *(int *)ctx);
	if (!found) { first = d->guidInstance; found = 1; }
	return DIENUM_CONTINUE;
}
static BOOL CALLBACK enumEff(const DIEFFECTINFOW *e, void *ctx) {
	printf("  effect: %ls type=%#lx\n", e->tszName, e->dwEffType);
	return DIENUM_CONTINUE;
}
static IDirectInputEffect *mk(IDirectInputDevice8W *dev, REFGUID g, const char *n, void *params, DWORD size) {
	static DWORD axes[1] = {DIJOFS_X}; static LONG dir[1] = {0};
	DIEFFECT e = {sizeof e};
	e.dwFlags = DIEFF_CARTESIAN | DIEFF_OBJECTOFFSETS;
	e.dwDuration = INFINITE; e.dwGain = DI_FFNOMINALMAX; e.dwTriggerButton = DIEB_NOTRIGGER;
	e.cAxes = 1; e.rgdwAxes = axes; e.rglDirection = dir;
	e.cbTypeSpecificParams = size; e.lpvTypeSpecificParams = params;
	IDirectInputEffect *eff = NULL;
	HRESULT hr = IDirectInputDevice8_CreateEffect(dev, g, &e, &eff, NULL);
	printf("CreateEffect %s: %#lx\n", n, hr);
	return eff;
}
static void run(IDirectInputEffect *eff, const char *n) {
	if (!eff) return;
	HRESULT hr = IDirectInputEffect_Start(eff, 1, 0); printf("Start %s: %#lx\n", n, hr);
	Sleep(300);
	hr = IDirectInputEffect_Stop(eff); printf("Stop %s: %#lx\n", n, hr);
}

int main(void) {
	freopen("Z:\\work\\fftest.log", "w", stdout); setvbuf(stdout, NULL, _IONBF, 0);
	HWND w = CreateWindowW(L"STATIC", L"fftest", WS_OVERLAPPEDWINDOW | WS_VISIBLE, 0, 0, 200, 100, 0, 0, 0, 0);
	SetForegroundWindow(w);
	HRESULT hr = DirectInput8Create(GetModuleHandleW(NULL), DIRECTINPUT_VERSION, &IID_IDirectInput8W, (void **)&di, NULL);
	printf("DirectInput8Create: %#lx\n", hr);
	int ff = 0; IDirectInput8_EnumDevices(di, DI8DEVCLASS_GAMECTRL, enumDev, &ff, DIEDFL_ATTACHEDONLY);
	for (int i = 0; i < 60 && !found; i++) { Sleep(500); IDirectInput8_EnumDevices(di, DI8DEVCLASS_GAMECTRL, enumDev, &ff, DIEDFL_ATTACHEDONLY); if (found) printf("hotplug seen after %d ms\n", (i + 1) * 500); }
	found = 0; ff = 1; IDirectInput8_EnumDevices(di, DI8DEVCLASS_GAMECTRL, enumDev, &ff, DIEDFL_ATTACHEDONLY | DIEDFL_FORCEFEEDBACK);
	if (!found) { printf("RESULT: no FF game controller\n"); return 1; }
	IDirectInputDevice8W *dev;
	hr = IDirectInput8_CreateDevice(di, &first, &dev, NULL); printf("CreateDevice: %#lx\n", hr);
	DIDEVCAPS caps = {sizeof caps}; IDirectInputDevice8_GetCapabilities(dev, &caps);
	printf("caps: flags=%#lx axes=%lu buttons=%lu povs=%lu ffdriver=%#lx\n", caps.dwFlags, caps.dwAxes, caps.dwButtons, caps.dwPOVs, caps.dwFFDriverVersion);
	IDirectInputDevice8_SetDataFormat(dev, &c_dfDIJoystick2);
	hr = IDirectInputDevice8_SetCooperativeLevel(dev, w, DISCL_EXCLUSIVE | DISCL_BACKGROUND); printf("SetCooperativeLevel: %#lx\n", hr);
	DIPROPDWORD ac = {{sizeof ac, sizeof ac.diph, 0, DIPH_DEVICE}, DIPROPAUTOCENTER_OFF};
	hr = IDirectInputDevice8_SetProperty(dev, DIPROP_AUTOCENTER, &ac.diph); printf("autocenter off: %#lx\n", hr);
	DIPROPDWORD g = {{sizeof g, sizeof g.diph, 0, DIPH_DEVICE}, 7500};
	hr = IDirectInputDevice8_SetProperty(dev, DIPROP_FFGAIN, &g.diph); printf("ffgain 7500: %#lx\n", hr);
	hr = IDirectInputDevice8_Acquire(dev); printf("Acquire: %#lx\n", hr);
	IDirectInputDevice8_EnumEffects(dev, enumEff, NULL, DIEFT_ALL);
	DICONSTANTFORCE cf = {-6000}; DICONDITION c = {0, 8000, 8000, 10000, 10000, 0};
	IDirectInputEffect *e1 = mk(dev, &GUID_ConstantForce, "constant", &cf, sizeof cf);
	IDirectInputEffect *e2 = mk(dev, &GUID_Spring, "spring", &c, sizeof c);
	IDirectInputEffect *e3 = mk(dev, &GUID_Damper, "damper", &c, sizeof c);
	run(e1, "constant"); run(e2, "spring"); run(e3, "damper");
	cf.lMagnitude = 3000; DIEFFECT u = {sizeof u}; u.cbTypeSpecificParams = sizeof cf; u.lpvTypeSpecificParams = &cf;
	if (e1) printf("SetParameters constant: %#lx\n", IDirectInputEffect_SetParameters(e1, &u, DIEP_TYPESPECIFICPARAMS | DIEP_START));
	Sleep(300);
	if (e1) IDirectInputEffect_Release(e1);
	if (e2) IDirectInputEffect_Release(e2);
	if (e3) IDirectInputEffect_Release(e3);
	DIJOYSTATE2 st;
	for (int i = 0; i < 10; i++) {
		Sleep(200); IDirectInputDevice8_Poll(dev);
		hr = IDirectInputDevice8_GetDeviceState(dev, sizeof st, &st);
		printf("state %d: %#lx lX=%ld lY=%ld b0=%#x\n", i, hr, st.lX, st.lY, st.rgbButtons[0]);
	}
	printf("state: %#lx lX=%ld lY=%ld lZ=%ld lRz=%ld b0=%#x pov=%ld\n", hr, st.lX, st.lY, st.lZ, st.lRz, st.rgbButtons[0], (long)st.rgdwPOV[0]);
	printf("RESULT: done\n");
	return 0;
}
