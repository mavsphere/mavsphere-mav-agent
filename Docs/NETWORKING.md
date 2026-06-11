# Raspberry Pi (Ubuntu) Networking Setup for MavSphere Agent

This document explains **supported networking setups** for the MavSphere Raspberry Pi agent, including a **safe default** and an **advanced optional configuration** to prefer a mobile hotspot when available.

The agent itself **does not manage networking**. These steps are for users who want more control, especially for development or outdoor use.

---

## 1. Recommended Default (Most Users)

### ✔ Use Raspberry Pi Imager Wi-Fi setup
When flashing Ubuntu using **Raspberry Pi Imager**, you can enter Wi-Fi credentials during image creation.

This results in:
- Wi-Fi managed by **netplan + cloud-init**
- A working connection on first boot
- No additional configuration required

This is the **recommended production setup**.

The MavSphere agent will work on:
- Ethernet
- Wi-Fi (any configuration)
- Static or DHCP

No networking changes are required.

---

## 2. When You Might Need More Control

You may want advanced Wi-Fi behaviour if:
- You want to **prefer a phone hotspot** when it is available
- You move the vehicle outdoors
- You want automatic fallback to home Wi-Fi
- You are developing and testing mobility scenarios

⚠️ **Warning**  
Changing networking on a headless Raspberry Pi can temporarily break connectivity.  
Always keep **Ethernet or USB tethering available** when doing this.

---

## 3. Advanced Setup: Prefer Hotspot When Available (Optional)

This method uses **NetworkManager**, which supports Wi-Fi priority.

### Summary
- Hotspot = higher priority
- Home Wi-Fi = lower priority
- Automatic fallback when hotspot disappears

---

## 4. Prerequisites

Ensure you have **temporary Ethernet connectivity** before proceeding.

---

## 5. Ensure Required Packages Are Installed

```bash
sudo apt update
sudo apt install -y network-manager iw rfkill linux-firmware
```

Reboot after installation:

```bash
sudo reboot
```

---

## 6. Ensure NetworkManager Is Active

```bash
systemctl is-active NetworkManager
```

Expected output:
```
active
```

---

## 7. Verify Wi-Fi Device Exists

```bash
ip link | grep wlan
```

Expected:
```
wlan0
```

Check radio state:
```bash
rfkill list
```

Unblock if necessary:
```bash
sudo rfkill unblock wifi
sudo nmcli radio wifi on
```

---

## 8. Scan for Networks

```bash
sudo nmcli dev wifi rescan
nmcli dev wifi list
```

Ensure both networks appear:
- Mobile hotspot (example: `19HOTMAV`)
- Home Wi-Fi (example: `VM4528196`)

If the hotspot does not appear:
- Set hotspot to **2.4 GHz**
- Use **WPA2-PSK**
- Disable **WPA3-only** or **5 GHz-only** modes

---

## 9. Create Clean Wi-Fi Profiles

Delete any partially created profiles first:

```bash
nmcli -t -f NAME,TYPE connection show | grep ':wifi'
sudo nmcli connection delete "<PROFILE_NAME>"
```

### Create hotspot profile
```bash
sudo nmcli connection add type wifi ifname wlan0 con-name "19HOTMAV" ssid "19HOTMAV"
sudo nmcli connection modify "19HOTMAV" \
  wifi-sec.key-mgmt wpa-psk \
  wifi-sec.psk "HOTSPOT_PASSWORD" \
  connection.autoconnect yes
```

### Create home Wi-Fi profile
```bash
sudo nmcli connection add type wifi ifname wlan0 con-name "HOME_WIFI" ssid "VM4528196"
sudo nmcli connection modify "HOME_WIFI" \
  wifi-sec.key-mgmt wpa-psk \
  wifi-sec.psk "HOME_WIFI_PASSWORD" \
  connection.autoconnect yes
```

---

## 10. Set Wi-Fi Priority (Hotspot Preferred)

Higher number = higher priority.

```bash
sudo nmcli connection modify "19HOTMAV" connection.autoconnect-priority 100
sudo nmcli connection modify "HOME_WIFI" connection.autoconnect-priority 50
```

Verify:
```bash
nmcli -f NAME,PRIORITY,DEVICE connection show
```

---

## 11. Test Behaviour

```bash
nmcli dev disconnect wlan0
nmcli dev connect wlan0
```

Expected:
- Hotspot ON → connects to hotspot
- Hotspot OFF → falls back to home Wi-Fi

---

## 12. (Optional) Simplify Netplan

To prevent netplan from generating partial Wi-Fi profiles, simplify it.

Edit `/etc/netplan/50-cloud-init.yaml`:

```yaml
network:
  version: 2
  renderer: NetworkManager
```

Apply:
```bash
sudo netplan apply
```

---

## 13. Recovery (If Networking Breaks)

If Wi-Fi stops working:
- Plug in **Ethernet**, or
- Use **USB tethering** from a phone
- Re-enable networking tools

This restores access immediately.

---

## 14. What the MavSphere Agent Assumes

- The agent **does not manage networking**
- Any working interface is acceptable
- Ethernet and Wi-Fi are treated equally
- Networking is intentionally left to the system

This avoids bricking headless installs and supports diverse environments.

---

## 15. Recommendation

- **Production / users**: use Pi Imager Wi-Fi or Ethernet
- **Developers / mobility testing**: use NetworkManager priorities
- Keep Ethernet available during experimentation
