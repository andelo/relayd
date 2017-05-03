#!/bin/sh

useradd -r -s /bin/false relayd
cp relayd /etc/init.d
chmod 755 /etc/init.d
mkdir /etc/relayd
cp relayd.con /etc/relayd
update-rc.d relayd defaults 3 6
